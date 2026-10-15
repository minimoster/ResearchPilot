package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yu-xin-c/Sea-mult-agent/docker-core/sandbox"
)

// This is an in-process Docker protocol fake, not a Docker integration test.
// It exercises the public Sandbox/Manager lifecycle, including hijacked attach.
func TestManagerPublicLifecycleCleansAllOwnedSnapshots(t *testing.T) {
	var mu sync.Mutex
	var created, commits int
	var refs, removedImages, events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.45")
		if strings.HasSuffix(path, "/attach") {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
			_ = rw.Flush()
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case path == "/containers/create":
			created++
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": fmt.Sprintf("container-%d", created)})
		case strings.HasSuffix(path, "/start"):
			w.WriteHeader(204)
		case path == "/commit":
			commits++
			refs = append(refs, r.URL.Query().Get("repo"))
			if commits == 4 {
				w.WriteHeader(500)
				_, _ = io.WriteString(w, `{"message":"commit failed"}`)
				return
			}
			w.WriteHeader(201)
			if commits == 5 {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": fmt.Sprintf("image-%d", commits)})
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
			events = append(events, "remove-container")
			w.WriteHeader(204)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/images/"):
			if r.URL.Query().Get("force") != "" && r.URL.Query().Get("force") != "0" {
				t.Errorf("forced removal: %s", r.URL.RawQuery)
			}
			events = append(events, "remove-image")
			removedImages = append(removedImages, strings.TrimPrefix(path, "/images/"))
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected fake Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", 500)
		}
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.45")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	cfg := sandbox.DefaultConfig()
	cfg.AuditLogDir = t.TempDir()
	sb, err := sandbox.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	m := NewManager(sb)
	if _, err := m.Commit(context.Background(), "same-node"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Commit(context.Background(), "same-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(context.Background(), "later-node"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(context.Background(), "failed-node"); err == nil {
		t.Fatal("expected failed commit")
	}
	if _, err := m.Commit(context.Background(), "empty-image-node"); err == nil {
		t.Fatal("expected error for a successful HTTP response with no image ID")
	}
	if _, ok := m.registry.GetByNodeID("empty-image-node"); ok {
		t.Fatal("empty image response was registered")
	}
	if _, ok := m.ownedImages[""]; ok {
		t.Fatal("empty image response established ownership")
	}
	if err := m.Rollback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if m.registry.Len() != 1 {
		t.Fatalf("rollback registry=%v", m.registry.All())
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(removedImages) != 3 {
		t.Fatalf("removed images=%v", removedImages)
	}
	if len(m.ownedImages) != 0 || m.registry.Len() != 0 {
		t.Fatal("cleanup retained owned snapshots")
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref == "" || seen[ref] {
			t.Fatalf("nonunique references=%v", refs)
		}
		seen[ref] = true
	}
	wantEvents := []string{"remove-container", "remove-container", "remove-image", "remove-image", "remove-image"}
	if fmt.Sprint(events) != fmt.Sprint(wantEvents) {
		t.Fatalf("events=%v", events)
	}
}
