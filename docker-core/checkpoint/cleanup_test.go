package checkpoint

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"
)

type imageTransport func(*http.Request) (*http.Response, error)

func (f imageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func imageResponse(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func imageClient(t *testing.T, transport imageTransport) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.45"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func TestCleanupRetainsOverwrittenAndRolledBackOwnership(t *testing.T) {
	m := NewManager(nil)
	m.recordSnapshot("first", "image-a", "ref-a")
	m.recordSnapshot("first", "image-b", "ref-b") // overwritten a stays owned
	m.recordSnapshot("second", "image-c", "ref-c")
	m.registry.RollbackTo("first") // rolled-back c stays owned
	m.registry.Register(Snapshot{NodeID: "external", ImageID: "base-image"})
	var removed []string
	m.cli = imageClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodDelete {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true" {
			t.Fatal("forced image removal")
		}
		if r.URL.Query().Get("noprune") != "1" {
			t.Fatalf("parent pruning enabled: %s", r.URL.RawQuery)
		}
		removed = append(removed, strings.TrimPrefix(r.URL.Path, "/v1.45/images/"))
		return imageResponse(200, `[]`)
	})
	if err := m.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"image-c", "image-b", "image-a"}; !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed=%v want=%v", removed, want)
	}
	if len(m.ownedImages) != 0 {
		t.Fatalf("ownership=%v", m.ownedImages)
	}
	if got := m.registry.All(); len(got) != 1 || got[0].ImageID != "base-image" {
		t.Fatalf("registry=%v", got)
	}
	if err := m.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Fatal("repeated cleanup repeated deletes")
	}
}

func TestCleanupFailureKeepsOwnershipAndContinues(t *testing.T) {
	m := NewManager(nil)
	m.recordSnapshot("first", "image-a", "a")
	m.recordSnapshot("second", "image-b", "b")
	fail := true
	var calls []string
	m.cli = imageClient(t, func(r *http.Request) (*http.Response, error) {
		id := strings.TrimPrefix(r.URL.Path, "/v1.45/images/")
		calls = append(calls, id)
		if id == "image-b" && fail {
			return imageResponse(409, `{"message":"image is used by a container"}`)
		}
		return imageResponse(200, `[]`)
	})
	if err := m.Cleanup(context.Background()); err == nil {
		t.Fatal("expected in-use conflict")
	}
	if len(m.ownedImages) != 1 || len(m.registry.All()) != 1 {
		t.Fatalf("owned=%v registry=%v", m.ownedImages, m.registry.All())
	}
	if _, ok := m.ownedImages["image-b"]; !ok {
		t.Fatal("lost ownership on failure")
	}
	fail = false
	if err := m.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"image-b", "image-a", "image-b"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestCleanupMissingImagesAreAlreadyRemoved(t *testing.T) {
	m := NewManager(nil)
	m.recordSnapshot("first", "missing", "ref")
	m.cli = imageClient(t, func(r *http.Request) (*http.Response, error) {
		return imageResponse(404, `{"message":"no such image"}`)
	})
	if err := m.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.ownedImages) != 0 || m.registry.Len() != 0 {
		t.Fatal("missing image retained")
	}
}

func TestCleanupCancellationPreservesOwnership(t *testing.T) {
	m := NewManager(nil)
	m.recordSnapshot("first", "image-a", "ref")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.cli = imageClient(t, func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if err := m.Cleanup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if len(m.ownedImages) != 1 || m.registry.Len() != 1 {
		t.Fatal("canceled cleanup lost ownership")
	}
}

func TestConcurrentCleanupIsSerialized(t *testing.T) {
	m := NewManager(nil)
	m.recordSnapshot("first", "image-a", "ref")
	var calls int
	m.cli = imageClient(t, func(r *http.Request) (*http.Response, error) { calls++; return imageResponse(200, `[]`) })
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Cleanup(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("deletes=%d", calls)
	}
}

func TestNilSandboxOperationsReturnErrors(t *testing.T) {
	m := NewManager(nil)
	if _, err := m.Commit(context.Background(), "node"); err == nil {
		t.Fatal("expected missing sandbox")
	}
	if err := m.Rollback(context.Background(), "image"); err == nil {
		t.Fatal("expected missing sandbox")
	}
	if err := m.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := (*Manager)(nil).Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}
