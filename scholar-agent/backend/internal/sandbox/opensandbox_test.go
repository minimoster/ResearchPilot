package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSandboxClientSendsInternalBearerToken(t *testing.T) {
	t.Setenv("SANDBOX_API_TOKEN", "internal-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer internal-secret" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SandboxCreateResponse{SandboxID: "dk-test"})
	}))
	defer server.Close()

	client := NewSandboxClient(server.URL)
	id, err := client.CreatePersistentSandbox(context.Background(), "task-1", "python:3.11", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "dk-test" {
		t.Fatalf("sandbox id=%q", id)
	}
}

func TestCleanupSandboxReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if err := NewSandboxClient(server.URL).CleanupSandbox(context.Background(), "id"); err == nil {
		t.Fatal("cleanup failure reported as success")
	}
}

func TestStreamAcceptsBoundedLargeFinalResponse(t *testing.T) {
	want := strings.Repeat("output", 350000)
	image := strings.Repeat("A", 2<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(executionStreamEvent{Type: "final", Response: &PythonRunResponse{Stdout: want, ExitCode: 0, Images: []string{image}}})
	}))
	defer server.Close()
	result, err := NewSandboxClient(server.URL).RunPythonCodeStream(context.Background(), "managed-id", "pass", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != want || len(result.Images) != 1 || result.Images[0] != image {
		t.Fatal("large final event was truncated")
	}
}

func TestStreamEventLimitCoversWorstCaseBoundedPayload(t *testing.T) {
	// Each output byte can become a six-byte JSON escape; an image expands 4/3.
	worstCase := 2*(8<<20)*6 + base64.StdEncoding.EncodedLen(8<<20) + 4096
	if maxExecutionStreamEventBytes < worstCase {
		t.Fatalf("event limit %d is below bounded response %d", maxExecutionStreamEventBytes, worstCase)
	}
}
