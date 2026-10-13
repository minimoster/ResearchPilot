package sandboxserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"scholar-agent-backend/internal/sandbox"
)

var containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type NativeDockerEngine struct {
	mu            sync.RWMutex
	mountPaths    map[string]string
	inspectLabels func(context.Context, string) (map[string]string, error)
}

type streamChunk struct {
	stream string
	line   string
}

type DockerHealth struct {
	Available           bool   `json:"available"`
	Command             string `json:"command"`
	ServerVersion       string `json:"server_version,omitempty"`
	GPURequest          string `json:"gpu_request,omitempty"`
	GPURuntimeAvailable bool   `json:"gpu_runtime_available"`
	Error               string `json:"error,omitempty"`
}

func NewNativeDockerEngine() *NativeDockerEngine {
	return &NativeDockerEngine{
		mountPaths:    make(map[string]string),
		inspectLabels: inspectDockerLabels,
	}
}

func (e *NativeDockerEngine) Health(ctx context.Context) DockerHealth {
	command := dockerCommand()
	gpuRequest := dockerGPURequest()
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	output, err := exec.CommandContext(checkCtx, command, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return DockerHealth{
			Available:  false,
			Command:    command,
			GPURequest: gpuRequest,
			Error:      fmt.Sprintf("Docker CLI or daemon is unavailable: %v, output: %s", err, strings.TrimSpace(string(output))),
		}
	}

	health := DockerHealth{
		Available:     true,
		Command:       command,
		ServerVersion: strings.TrimSpace(string(output)),
		GPURequest:    gpuRequest,
	}
	if gpuRequest == "" {
		return health
	}

	runtimes, runtimeErr := exec.CommandContext(checkCtx, command, "info", "--format", "{{json .Runtimes}}").CombinedOutput()
	health.GPURuntimeAvailable = runtimeErr == nil && strings.Contains(string(runtimes), `"nvidia"`)
	if !health.GPURuntimeAvailable {
		health.Available = false
		health.Error = fmt.Sprintf("GPU sandbox requested (%s), but the NVIDIA Docker runtime is unavailable", gpuRequest)
	}
	return health
}

func dockerGPURequest() string {
	request := strings.TrimSpace(os.Getenv("SANDBOX_DOCKER_GPUS"))
	if strings.EqualFold(request, "none") {
		return ""
	}
	return request
}

func dockerCommand() string {
	if path, err := exec.LookPath("docker"); err == nil {
		return path
	}
	for _, path := range []string{
		"/usr/local/bin/docker",
		"/opt/homebrew/bin/docker",
		"/Applications/Docker.app/Contents/Resources/bin/docker",
		"/Volumes/Docker/Docker.app/Contents/Resources/bin/docker",
	} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return "docker"
}

func (e *NativeDockerEngine) Create(ctx context.Context, image string, mountPath string) (string, error) {
	args, err := secureDockerRunArgs(image)
	if err != nil {
		return "", err
	}
	if gpuRequest := dockerGPURequest(); gpuRequest != "" {
		args = append(args, "--gpus", gpuRequest)
	}
	if mountPath != "" {
		normalizedMountPath, err := normalizeDockerMountPath(mountPath)
		if err != nil {
			return "", err
		}
		mountPath = normalizedMountPath
		args = append(args, "-v", fmt.Sprintf("%s:/workspace", normalizedMountPath))
	}
	args = append(args, image, "sleep", "infinity")

	cmd := exec.CommandContext(ctx, dockerCommand(), args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run failed: %v, output: %s", err, string(output))
	}

	containerID := extractContainerIDFromDockerRunOutput(string(output))
	if containerID == "" {
		return "", fmt.Errorf("docker run returned empty container id, output: %s", string(output))
	}
	e.mu.Lock()
	e.mountPaths[containerID] = mountPath
	e.mu.Unlock()
	return containerID, nil
}

// validateSandboxID prevents API-supplied values from becoming Docker CLI options.
// Ownership authorization additionally requires the persistent managed label below.
func validateSandboxID(id string) error {
	if !containerIDPattern.MatchString(id) {
		return fmt.Errorf("invalid sandbox id %q", id)
	}
	return nil
}

// managedSandboxLabel is shared by the standalone and embedded sandbox services.
// Containers created before this label was introduced must be recreated; they
// are deliberately not adopted automatically during an upgrade.
const managedSandboxLabel = "org.openresearch.sandbox.managed"

func inspectDockerLabels(ctx context.Context, id string) (map[string]string, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(inspectCtx, dockerCommand(), "inspect", "--type", "container", "--format", "{{json .Config.Labels}}", id).Output()
	if err != nil {
		return nil, fmt.Errorf("inspect sandbox ownership: %w", err)
	}
	var labels map[string]string
	if err := json.Unmarshal(output, &labels); err != nil {
		return nil, fmt.Errorf("decode sandbox labels: %w", err)
	}
	return labels, nil
}

func (e *NativeDockerEngine) requireManagedSandbox(ctx context.Context, id string) error {
	if err := validateSandboxID(id); err != nil {
		return err
	}
	inspect := e.inspectLabels
	if inspect == nil {
		inspect = inspectDockerLabels
	}
	labels, err := inspect(ctx, id)
	if err != nil {
		return err
	}
	if labels[managedSandboxLabel] != "true" {
		return fmt.Errorf("container %s is not managed by this sandbox service; recreate legacy unlabeled sandboxes", id)
	}
	return nil
}

func (e *NativeDockerEngine) Delete(ctx context.Context, id string) error {
	if err := e.requireManagedSandbox(ctx, id); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, dockerCommand(), "rm", "-f", id).Run(); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.mountPaths, id)
	e.mu.Unlock()
	return nil
}

func (e *NativeDockerEngine) ExecutePython(ctx context.Context, id string, code string) (*ExecutionResponse, error) {
	if err := e.requireManagedSandbox(ctx, id); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, dockerCommand(), "exec", id, "python3", "-c", code)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitCode := resolveExitCode(err)
	response := &ExecutionResponse{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}

	// 兼容已有逻辑：如果代码把图像保存到挂载目录，就顺手带回前端展示。
	e.attachOutputImage(id, response)
	return response, nil
}

func (e *NativeDockerEngine) ExecutePythonStream(ctx context.Context, id string, code string, emit func(ExecutionStreamEvent) error) (*ExecutionResponse, error) {
	if err := e.requireManagedSandbox(ctx, id); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, dockerCommand(), "exec", id, "python3", "-c", code)
	response, err := e.runStreamingCommand(cmd, emit)
	if err != nil {
		return nil, err
	}
	e.attachOutputImage(id, response)
	if emit == nil {
		return response, nil
	}
	return response, emit(ExecutionStreamEvent{
		Type:     "final",
		Response: response,
	})
}

func (e *NativeDockerEngine) ExecuteCommand(ctx context.Context, id string, cmdArr []string) (*ExecutionResponse, error) {
	if err := e.requireManagedSandbox(ctx, id); err != nil {
		return nil, err
	}
	args := append([]string{"exec", id}, cmdArr...)
	cmd := exec.CommandContext(ctx, dockerCommand(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return &ExecutionResponse{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: resolveExitCode(err),
	}, nil
}

func (e *NativeDockerEngine) ExecuteCommandStream(ctx context.Context, id string, cmdArr []string, emit func(ExecutionStreamEvent) error) (*ExecutionResponse, error) {
	if err := e.requireManagedSandbox(ctx, id); err != nil {
		return nil, err
	}
	args := append([]string{"exec", id}, cmdArr...)
	cmd := exec.CommandContext(ctx, dockerCommand(), args...)
	response, err := e.runStreamingCommand(cmd, emit)
	if err != nil {
		return nil, err
	}
	if emit == nil {
		return response, nil
	}
	return response, emit(ExecutionStreamEvent{
		Type:     "final",
		Response: response,
	})
}

func (e *NativeDockerEngine) attachOutputImage(id string, response *ExecutionResponse) {
	e.mu.RLock()
	mountPath, ok := e.mountPaths[id]
	e.mu.RUnlock()
	if !ok {
		return
	}

	imgData, err := sandbox.ReadOutputImage(mountPath)
	if err != nil {
		return
	}
	response.Images = []string{base64.StdEncoding.EncodeToString(imgData)}
}

func (e *NativeDockerEngine) runStreamingCommand(cmd *exec.Cmd, emit func(ExecutionStreamEvent) error) (*ExecutionResponse, error) {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer stdoutPipe.Close()
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	defer stderrPipe.Close()
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	chunks := make(chan streamChunk, 64)
	done := make(chan struct{})
	readErrors := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); readErrors <- readPipeLines(stdoutPipe, "stdout", &stdoutBuf, chunks, done) }()
	go func() { defer wg.Done(); readErrors <- readPipeLines(stderrPipe, "stderr", &stderrBuf, chunks, done) }()
	go func() { wg.Wait(); close(chunks) }()

	// Always reap the CLI and unblock readers, even when the HTTP writer fails.
	// Killing this CLI alone does not stop a Docker daemon-side exec process.
	waited := false
	defer func() {
		close(done)
		if !waited {
			_ = cmd.Process.Kill()
			_ = stdoutPipe.Close()
			_ = stderrPipe.Close()
			_ = cmd.Wait()
		}
		wg.Wait()
	}()
	readersDone := 0
	for chunks != nil || readersDone < 2 {
		select {
		case readErr := <-readErrors:
			readersDone++
			if readErr != nil {
				return nil, fmt.Errorf("read command output: %w", readErr)
			}
		case chunk, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			if emit != nil {
				if err := emit(ExecutionStreamEvent{Type: "chunk", Stream: chunk.stream, Message: chunk.line}); err != nil {
					return nil, err
				}
			}
		}
	}
	waitErr := cmd.Wait()
	waited = true
	return &ExecutionResponse{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String(), ExitCode: resolveExitCode(waitErr)}, nil
}

func extractContainerIDFromDockerRunOutput(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if containerIDPattern.MatchString(line) {
			return line
		}
	}
	return ""
}

func normalizeDockerMountPath(mountPath string) (string, error) {
	if strings.TrimSpace(mountPath) == "" {
		return "", nil
	}
	return normalizeAndAuthorizeMountPath(mountPath)
}

func resolveExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		return exitError.ExitCode()
	}
	return -1
}

func readPipeLines(reader io.Reader, stream string, sink *bytes.Buffer, out chan<- streamChunk, done <-chan struct{}) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if sink.Len()+len(line)+1 > 8<<20 {
			return fmt.Errorf("command output exceeds 8 MiB limit")
		}
		sink.WriteString(line)
		sink.WriteByte('\n')
		select {
		case out <- streamChunk{stream: stream, line: line}:
		case <-done:
			return nil
		}
	}
	return scanner.Err()
}
