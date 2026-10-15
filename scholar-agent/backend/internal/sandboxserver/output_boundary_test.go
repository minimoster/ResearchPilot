package sandboxserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutputImageCannotReadHostFileThroughSymlink(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "host-fixture.txt")
	if err := os.WriteFile(outside, []byte("host-only-regression-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "output_plot.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	engine := &NativeDockerEngine{mountPaths: map[string]string{"test-container": workspace}}
	response := &ExecutionResponse{}
	engine.attachOutputImage("test-container", response)
	if len(response.Images) != 0 {
		t.Fatal("sandbox output symlink exposed a host file as an image")
	}
}
