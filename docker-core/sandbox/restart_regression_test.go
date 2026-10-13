package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

type lifecycleRoundTripper func(*http.Request) (*http.Response, error)

func (f lifecycleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func lifecycleResponse(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestRestartPreservesOriginalUntilReplacementReady(t *testing.T) {
	for _, tc := range []struct {
		name, failAt string
		wantCalls    []string
		wantID       string
	}{
		{"create failure", "create", []string{"create"}, "old"},
		{"start failure", "start", []string{"create", "start", "remove-new"}, "old"},
		{"old removal failure", "remove-old", []string{"create", "start", "remove-old", "remove-new"}, "old"},
		{"success", "", []string{"create", "start", "remove-old"}, "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			cfg := DefaultConfig()
			cfg.Image = "base"
			var e *Engine
			cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.45"), client.WithHTTPClient(&http.Client{Transport: lifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
				var op string
				switch {
				case strings.HasSuffix(req.URL.Path, "/containers/create"):
					op = "create"
					var got struct{ Image string }
					if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
						t.Fatal(err)
					}
					if got.Image != "snapshot" {
						t.Fatalf("replacement image=%q", got.Image)
					}
				case strings.HasSuffix(req.URL.Path, "/containers/new/start"):
					op = "start"
				case strings.HasSuffix(req.URL.Path, "/containers/old"):
					op = "remove-old"
				case strings.HasSuffix(req.URL.Path, "/containers/new"):
					op = "remove-new"
				default:
					t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				calls = append(calls, op)
				if e.ContainerID() != "old" || e.Config().Image != "base" {
					t.Fatal("engine switched before replacement was ready and old removal succeeded")
				}
				if op == tc.failAt {
					return lifecycleResponse(500, `{"message":"injected failure"}`)
				}
				if op == "create" {
					return lifecycleResponse(201, `{"Id":"new"}`)
				}
				return lifecycleResponse(204, "")
			})}))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			e = &Engine{cli: cli, containerID: "old", config: cfg}
			id, err := e.RestartFrom(context.Background(), "snapshot")
			if (err != nil) != (tc.failAt != "") {
				t.Fatalf("id=%q error=%v", id, err)
			}
			if e.ContainerID() != tc.wantID {
				t.Fatalf("container=%q want=%q", e.ContainerID(), tc.wantID)
			}
			wantImage := "base"
			if tc.failAt == "" {
				wantImage = "snapshot"
			}
			if e.Config().Image != wantImage {
				t.Fatalf("image=%q want=%q", e.Config().Image, wantImage)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("calls=%v want=%v", calls, tc.wantCalls)
			}
		})
	}
}

func TestRestartCanceledOldRemovalCleansReplacementWithLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removed := false
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.45"), client.WithHTTPClient(&http.Client{Transport: lifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/containers/create"):
			return lifecycleResponse(201, `{"Id":"new"}`)
		case strings.HasSuffix(req.URL.Path, "/start"):
			return lifecycleResponse(204, "")
		case strings.HasSuffix(req.URL.Path, "/containers/old"):
			cancel()
			return nil, context.Canceled
		case strings.HasSuffix(req.URL.Path, "/containers/new"):
			if req.Context().Err() != nil {
				t.Fatal("cleanup inherited cancellation")
			}
			if _, ok := req.Context().Deadline(); !ok {
				t.Fatal("cleanup missing deadline")
			}
			removed = true
			return lifecycleResponse(204, "")
		default:
			return nil, errors.New("unexpected request")
		}
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	e := &Engine{cli: cli, containerID: "old", config: DefaultConfig()}
	if _, err := e.RestartFrom(ctx, "snapshot"); err == nil {
		t.Fatal("expected cancellation")
	}
	if !removed || e.ContainerID() != "old" {
		t.Fatalf("removed=%v id=%q", removed, e.ContainerID())
	}
}

func TestRestartFailedCleanupRetriedOnDestroy(t *testing.T) {
	fail := true
	var removed []string
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.45"), client.WithHTTPClient(&http.Client{Transport: lifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/containers/create"):
			return lifecycleResponse(201, `{"Id":"new"}`)
		case strings.HasSuffix(req.URL.Path, "/start"):
			return lifecycleResponse(204, "")
		case req.Method == http.MethodDelete:
			if fail {
				return lifecycleResponse(500, `{"message":"busy"}`)
			}
			removed = append(removed, req.URL.Path)
			return lifecycleResponse(204, "")
		default:
			return nil, errors.New("unexpected request")
		}
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	e := &Engine{cli: cli, containerID: "old", config: DefaultConfig()}
	if _, err := e.RestartFrom(context.Background(), "snapshot"); err == nil {
		t.Fatal("expected removal failure")
	}
	if len(e.pendingContainers) != 1 {
		t.Fatalf("pending=%v", e.pendingContainers)
	}
	fail = false
	if err := e.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || len(e.pendingContainers) != 0 {
		t.Fatalf("removed=%v pending=%v", removed, e.pendingContainers)
	}
}

func TestDestroyAlreadyMissingContainer(t *testing.T) {
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.45"), client.WithHTTPClient(&http.Client{Transport: lifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
		return lifecycleResponse(404, `{"message":"no such container"}`)
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	e := &Engine{cli: cli, containerID: "gone"}
	if err := e.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.ContainerID() != "" {
		t.Fatal("stale container id")
	}
}

func newLifecycleClient(t *testing.T, transport lifecycleRoundTripper) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.45"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func TestRestartStartFailureRetainsFailedCleanupForDestroy(t *testing.T) {
	fail := true
	var removed []string
	cli := newLifecycleClient(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/containers/create"):
			return lifecycleResponse(201, `{"Id":"new"}`)
		case strings.HasSuffix(req.URL.Path, "/start"):
			return lifecycleResponse(500, `{"message":"start failed"}`)
		case req.Method == http.MethodDelete:
			if fail {
				return lifecycleResponse(500, `{"message":"cleanup failed"}`)
			}
			removed = append(removed, req.URL.Path)
			return lifecycleResponse(204, "")
		default:
			return nil, errors.New("unexpected request")
		}
	})
	e := &Engine{cli: cli, containerID: "old", config: DefaultConfig()}
	if _, err := e.RestartFrom(context.Background(), "snapshot"); err == nil {
		t.Fatal("expected start failure")
	}
	if e.ContainerID() != "old" || len(e.pendingContainers) != 1 {
		t.Fatalf("id=%q pending=%v", e.ContainerID(), e.pendingContainers)
	}
	fail = false
	if err := e.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v", removed)
	}
}
