package engine

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// ReadOutputImage reads and removes a bounded, regular output file without
// allowing sandbox-controlled symlinks to escape the authorized workspace.
func ReadOutputImage(workspace string) ([]byte, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat("output_plot.png")
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("output image is not a regular file")
	}
	file, err := root.OpenFile("output_plot.png", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	const maxImageBytes = 8 << 20
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxImageBytes {
		return nil, fmt.Errorf("output image is not a bounded regular file")
	}
	imgData, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(imgData) > maxImageBytes {
		return nil, fmt.Errorf("output image exceeds size limit")
	}
	// Close first so removal also works on platforms that prohibit deleting open files.
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := root.Remove("output_plot.png"); err != nil {
		return nil, err
	}
	return imgData, nil
}
