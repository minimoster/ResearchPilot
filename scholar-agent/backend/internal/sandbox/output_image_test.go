package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOutputImageRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("host secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "output_plot.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if data, err := ReadOutputImage(workspace); err == nil || len(data) != 0 {
		t.Fatalf("escaped workspace: %q, %v", data, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "host secret" {
		t.Fatal("outside file was modified")
	}
}

func TestReadOutputImageRegularAndBounded(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "output_plot.png")
	if err := os.WriteFile(path, []byte("image bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadOutputImage(workspace)
	if err != nil || string(data) != "image bytes" {
		t.Fatalf("read: %q, %v", data, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("output was not removed")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (8<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOutputImage(workspace); err == nil {
		t.Fatal("oversized output accepted")
	}
}
