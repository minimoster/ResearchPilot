package sandbox

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestSandboxCloseRunsAllCleanupHooksAfterRemoval(t *testing.T) {
	var calls []string
	cli := newLifecycleClient(t, func(req *http.Request) (*http.Response, error) {
		calls = append(calls, "remove-container")
		return lifecycleResponse(204, "")
	})
	sb := &Sandbox{engine: &Engine{cli: cli, containerID: "old"}}
	firstError := errors.New("first cleanup failed")
	sb.AddCleanup(nil)
	sb.AddCleanup(func(ctx context.Context) error {
		if sb.Engine().ContainerID() != "" {
			t.Fatal("cleanup before container removal")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("cleanup missing deadline")
		}
		// The callback must not hold the registration lock.
		sb.AddCleanup(func(context.Context) error { return nil })
		calls = append(calls, "first")
		return firstError
	})
	sb.AddCleanup(func(ctx context.Context) error { calls = append(calls, "second"); return nil })
	if err := sb.Close(); !errors.Is(err, firstError) {
		t.Fatalf("error=%v", err)
	}
	if want := []string{"remove-container", "first", "second"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v", calls)
	}
	if err := sb.Close(); !errors.Is(err, firstError) {
		t.Fatalf("retry error=%v", err)
	}
	if want := []string{"remove-container", "first", "second", "first", "second"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("retry calls=%v", calls)
	}
}

func TestSandboxCloseDefersHooksWhileContainerRemovalFails(t *testing.T) {
	fail := true
	cli := newLifecycleClient(t, func(req *http.Request) (*http.Response, error) {
		if fail {
			return lifecycleResponse(500, `{"message":"cannot remove"}`)
		}
		return lifecycleResponse(204, "")
	})
	sb := &Sandbox{engine: &Engine{cli: cli, containerID: "old"}}
	called := false
	sb.AddCleanup(func(context.Context) error { called = true; return nil })
	if err := sb.Close(); err == nil {
		t.Fatal("expected remove failure")
	}
	if called {
		t.Fatal("dependent cleanup ran before successful removal")
	}
	fail = false
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("cleanup not retried")
	}
}

func TestExecuteWithoutSessionReturnsError(t *testing.T) {
	if _, err := (&Sandbox{}).Execute(context.Background(), "echo hi"); err == nil {
		t.Fatal("expected missing session")
	}
}
