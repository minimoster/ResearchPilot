package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitizeUploadNameHandlesLongExtensions(t *testing.T) {
	for _, name := range []string{"a." + strings.Repeat("x", 200), "." + strings.Repeat("x", 200), strings.Repeat("文", 100) + ".csv"} {
		got := sanitizeUploadName(name)
		if len(got) > 180 || !utf8.ValidString(got) {
			t.Fatalf("sanitized name must be bounded valid UTF-8: %q (%d bytes)", got, len(got))
		}
	}
	name := sanitizeUploadName(strings.Repeat("文", 100) + ".csv")
	if !strings.HasSuffix(name, ".csv") {
		t.Fatalf("lost permitted extension: %q", name)
	}
}

func TestUploadExcerptIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2*uploadTextExcerptLimit)), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readUploadExcerpt(path)
	if err != nil || len(got) != uploadTextExcerptLimit {
		t.Fatalf("excerpt bytes=%d err=%v", len(got), err)
	}
}

func TestExecutionLogDoesNotBlockCleanup(t *testing.T) {
	logs := make(chan string, 1)
	logs <- "full"
	done := make(chan struct{})
	go func() { sendExecutionLog(context.Background(), logs, "cleanup"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("logging blocked behind disconnected stream")
	}
}
