package sandboxserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Only this test binary is spawned. No Docker executable or daemon is used.
func TestSandboxProcessHelper(t *testing.T) {
	mode := os.Getenv("OPENRESEARCH_TEST_PROCESS")
	if mode == "" {
		return
	}
	if mode == "long" {
		fmt.Print(strings.Repeat("x", 2<<20))
	} else {
		for i := 0; i < 512; i++ {
			fmt.Println("line")
		}
	}
	os.Exit(0)
}

func helperCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSandboxProcessHelper$")
	cmd.Env = append(os.Environ(), "OPENRESEARCH_TEST_PROCESS="+mode)
	return cmd
}

func TestStreamingEmitErrorReapsProcess(t *testing.T) {
	sentinel := errors.New("client disconnected")
	cmd := helperCommand(t, "lines")
	_, err := NewNativeDockerEngine().runStreamingCommand(cmd, func(ExecutionStreamEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error=%v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("child was not waited on")
	}
}

func TestStreamingLongLineReturnsError(t *testing.T) {
	cmd := helperCommand(t, "long")
	_, err := NewNativeDockerEngine().runStreamingCommand(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("error=%v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("child was not waited on")
	}
}

func TestStreamingNilEmitter(t *testing.T) {
	result, err := NewNativeDockerEngine().runStreamingCommand(helperCommand(t, "lines"), nil)
	if err != nil || result.ExitCode != 0 || strings.Count(result.Stdout, "line") != 512 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestContainerIDParserRejectsWarningsAndOptions(t *testing.T) {
	id := strings.Repeat("a", 64)
	if got := extractContainerIDFromDockerRunOutput(id + "\nWARNING: extra output\n"); got != id {
		t.Fatalf("id=%q", got)
	}
	for _, value := range []string{"warning", "--privileged", "", "victim-container"} {
		if err := validateSandboxID(value); err == nil {
			t.Fatalf("accepted id %q", value)
		}
	}
}

func TestDockerImageCannotSupplyFlags(t *testing.T) {
	t.Setenv("SANDBOX_IMAGE_ALLOWLIST", "")
	for _, image := range []string{"--privileged", "--network=host", "python latest"} {
		if _, err := secureDockerRunArgs(image); err == nil {
			t.Fatalf("accepted image %q", image)
		}
	}
}

func TestOpenSandboxHonorsCancellationAndHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"unavailable"}`)
	}))
	defer server.Close()
	e := NewOpenSandboxEngine(server.URL)
	if _, err := e.ExecutePython(context.Background(), "id", "pass"); err == nil {
		t.Fatal("upstream failure reported as success")
	}
	if _, err := e.ExecuteCommand(context.Background(), "id", []string{"true"}); err == nil {
		t.Fatal("upstream failure reported as success")
	}
	if err := e.Delete(context.Background(), "id"); err == nil {
		t.Fatal("delete failure reported as success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Create(ctx, "python:3.11", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("create cancellation: %v", err)
	}
	if _, err := e.ExecutePython(ctx, "id", "pass"); !errors.Is(err, context.Canceled) {
		t.Fatalf("execute cancellation: %v", err)
	}
}

func TestPersistentManagedLabelAndUnmanagedRejection(t *testing.T) {
	t.Setenv("SANDBOX_IMAGE_ALLOWLIST", "")
	args, err := secureDockerRunArgs("python:3.11")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--label "+managedSandboxLabel+"=true") {
		t.Fatalf("missing persistent ownership label: %v", args)
	}
	id := strings.Repeat("a", 64)
	e := NewNativeDockerEngine()
	inspections := 0
	e.inspectLabels = func(ctx context.Context, got string) (map[string]string, error) {
		inspections++
		if got != id {
			t.Fatalf("inspected id=%q", got)
		}
		return nil, nil
	}
	// Each entry point must stop before spawning an exec/rm command. These tests
	// never invoke the Docker CLI, including when checking pre-upgrade containers.
	if err := e.Delete(context.Background(), id); err == nil {
		t.Fatal("deleted unmanaged container")
	}
	if _, err := e.ExecutePython(context.Background(), id, "pass"); err == nil {
		t.Fatal("executed in unmanaged container")
	}
	if _, err := e.ExecutePythonStream(context.Background(), id, "pass", nil); err == nil {
		t.Fatal("streamed in unmanaged container")
	}
	if _, err := e.ExecuteCommand(context.Background(), id, []string{"true"}); err == nil {
		t.Fatal("executed command in unmanaged container")
	}
	if _, err := e.ExecuteCommandStream(context.Background(), id, []string{"true"}, nil); err == nil {
		t.Fatal("streamed command in unmanaged container")
	}
	if inspections != 5 {
		t.Fatalf("inspections=%d", inspections)
	}
	// A fresh engine accepts a labeled container without an in-memory registry.
	e = NewNativeDockerEngine()
	e.inspectLabels = func(context.Context, string) (map[string]string, error) {
		return map[string]string{managedSandboxLabel: "true"}, nil
	}
	if err := e.requireManagedSandbox(context.Background(), id); err != nil {
		t.Fatalf("restart recovery failed: %v", err)
	}
}

func TestOwnershipInspectionFailureFailsClosed(t *testing.T) {
	e := NewNativeDockerEngine()
	sentinel := errors.New("inspect unavailable")
	e.inspectLabels = func(context.Context, string) (map[string]string, error) { return nil, sentinel }
	if err := e.requireManagedSandbox(context.Background(), strings.Repeat("b", 64)); !errors.Is(err, sentinel) {
		t.Fatalf("error=%v", err)
	}
	if err := e.requireManagedSandbox(context.Background(), "--privileged"); err == nil || errors.Is(err, sentinel) {
		t.Fatalf("invalid id reached inspect: %v", err)
	}
}

func TestOpenSandboxRejectsUnsupportedHostMount(t *testing.T) {
	// An unusable URL proves rejection happens before any upstream request.
	e := NewOpenSandboxEngine(":invalid")
	if _, err := e.Create(context.Background(), "python:3.11", "/workspace/task"); err == nil || !strings.Contains(err.Error(), "does not support host workspace mounts") {
		t.Fatalf("error=%v", err)
	}
}
