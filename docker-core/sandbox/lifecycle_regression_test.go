package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

type cleanupTransport struct {
	cancel  context.CancelFunc
	removed bool
}

func (t *cleanupTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/containers/create"):
		return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Id":"test-container","Warnings":[]}`))}, nil
	case strings.HasSuffix(req.URL.Path, "/start"):
		t.cancel()
		return nil, context.Canceled
	case req.Method == http.MethodDelete:
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		t.removed = true
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	default:
		return nil, errors.New("unexpected Docker API request")
	}
}

func TestCreateFailureCleansUpWithLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &cleanupTransport{cancel: cancel}
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.45"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := createContainer(ctx, cli, DefaultConfig()); err == nil {
		t.Fatal("expected start cancellation")
	}
	if !transport.removed {
		t.Fatal("container cleanup did not survive canceled request")
	}
}

type delayedInterruptWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *delayedInterruptWriter) Write(p []byte) (int, error) {
	if string(p) == "\x03\n" {
		w.once.Do(func() { close(w.started) })
		<-w.release
	}
	return len(p), nil
}
func (w *delayedInterruptWriter) Close() error { return nil }

func TestSessionCancellationWaitsForInterrupt(t *testing.T) {
	writer := &delayedInterruptWriter{started: make(chan struct{}), release: make(chan struct{})}
	s := &Session{conn: writer, splitter: &StreamSplitter{notify: make(chan struct{}, 1)}, envMap: map[string]string{}, envSynced: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { defer close(finished); _, _ = s.Execute(ctx, "sleep 100") }()
	// Wait until Execute holds the session lock and has started its watcher.
	deadline := time.After(2 * time.Second)
	for s.mu.TryLock() {
		s.mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("Execute did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-writer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not start")
	}
	select {
	case <-finished:
		t.Fatal("session returned before interrupt was sent")
	case <-time.After(20 * time.Millisecond):
	}
	close(writer.release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not finish")
	}
}

func TestSessionAlreadyCanceledDoesNotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Session{}).Execute(ctx, "touch should-not-run"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
