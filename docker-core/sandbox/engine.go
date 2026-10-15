package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Engine 管理 Docker 容器的生命周期。Callers must serialize lifecycle operations.
type Engine struct {
	cli         *client.Client
	containerID string
	config      Config
	// Failed replacement cleanup is retried by Destroy.
	pendingContainers []string
}

// containerCleanupError retains a created container that could not be removed.
// RestartFrom keeps it in its lifecycle so Destroy can retry cleanup.
type containerCleanupError struct {
	containerID string
	err         error
}

func (e *containerCleanupError) Error() string { return e.err.Error() }
func (e *containerCleanupError) Unwrap() error { return e.err }

// NewEngine 创建 Docker Client 并拉起容器
func NewEngine(ctx context.Context, cfg Config) (*Engine, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	engine, err := createContainer(ctx, cli, cfg)
	if err != nil {
		cli.Close()
		return nil, err
	}
	engine.config = cfg
	return engine, nil
}

// newEngineWithClient 使用已有的 Docker Client 创建 Engine（内部使用，用于 RestartFrom）
func newEngineWithClient(ctx context.Context, cli *client.Client, cfg Config) (*Engine, error) {
	return createContainer(ctx, cli, cfg)
}

func createContainer(ctx context.Context, cli *client.Client, cfg Config) (*Engine, error) {
	// 容器配置：开启 TTY + Stdin，保持伪终端长连接
	containerCfg := &container.Config{
		Image:      cfg.Image,
		Tty:        true,
		OpenStdin:  true,
		WorkingDir: cfg.WorkingDir,
		Cmd:        []string{"/bin/bash"},
	}

	// 注入初始环境变量
	for k, v := range cfg.EnvVars {
		containerCfg.Env = append(containerCfg.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// 宿主机配置：运行时、资源限制
	hostCfg := &container.HostConfig{
		Resources: container.Resources{
			Memory:   cfg.MemoryLimit,
			CPUQuota: cfg.CPUQuota,
		},
	}

	// 指定运行时（gVisor runsc 或默认 runc）
	if cfg.Runtime != "" {
		hostCfg.Runtime = cfg.Runtime
	}

	// GPU 透传
	if cfg.GPUEnabled {
		hostCfg.DeviceRequests = []container.DeviceRequest{
			{
				Capabilities: [][]string{{"gpu"}},
				Count:        -1, // 所有可用 GPU
			},
		}
	}

	resp, err := cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// 启动失败则清理已创建的容器
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cleanupErr := cli.ContainerRemove(cleanupCtx, resp.ID, container.RemoveOptions{Force: true}); cleanupErr != nil && !client.IsErrNotFound(cleanupErr) {
			return nil, &containerCleanupError{containerID: resp.ID, err: errors.Join(
				fmt.Errorf("failed to start container: %w", err),
				fmt.Errorf("failed to clean up container %s: %w", resp.ID, cleanupErr),
			)}
		}
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	return &Engine{
		cli:         cli,
		containerID: resp.ID,
		config:      cfg,
	}, nil
}

// ContainerID 返回当前容器 ID（暴露给 checkpoint 使用）
func (e *Engine) ContainerID() string {
	return e.containerID
}

// Client 返回底层 Docker Client（暴露给 checkpoint 使用）
func (e *Engine) Client() *client.Client {
	return e.cli
}

// Config 返回当前配置
func (e *Engine) Config() Config {
	return e.config
}

// removeContainer 仅移除容器，不关闭 Docker 客户端（供 RestartFrom 内部使用）
func (e *Engine) removeContainer(ctx context.Context) error {
	if e.containerID == "" {
		return nil
	}
	err := e.cli.ContainerRemove(ctx, e.containerID, container.RemoveOptions{Force: true})
	if err != nil && !client.IsErrNotFound(err) {
		return fmt.Errorf("failed to remove container %s: %w", e.containerID, err)
	}
	e.containerID = ""
	return nil
}

// removeContainers leaves the Docker client available for dependent cleanup.
func (e *Engine) removeContainers(ctx context.Context) error {
	var cleanupErrors []error
	if err := e.removeContainer(ctx); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	remaining := e.pendingContainers[:0]
	for _, id := range e.pendingContainers {
		err := e.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
		if err != nil && !client.IsErrNotFound(err) {
			remaining = append(remaining, id)
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to remove replacement container %s: %w", id, err))
		}
	}
	e.pendingContainers = remaining
	return errors.Join(cleanupErrors...)
}

// Destroy 强制销毁容器并关闭 Docker 客户端，释放所有资源
func (e *Engine) Destroy(ctx context.Context) error {
	cleanupErr := e.removeContainers(ctx)
	if e.cli != nil {
		return errors.Join(cleanupErr, e.cli.Close())
	}
	return cleanupErr
}

func (e *Engine) closeClient() error {
	if e.cli != nil {
		return e.cli.Close()
	}
	return nil
}

// RestartFrom 基于指定镜像重建容器（checkpoint 回滚时调用）
// Prepare and start the replacement before removing the current container.
func (e *Engine) RestartFrom(ctx context.Context, imageID string) (string, error) {
	// 用快照镜像重建，复用原有配置
	newCfg := e.config
	newCfg.Image = imageID

	newEngine, err := newEngineWithClient(ctx, e.cli, newCfg)
	if err != nil {
		var cleanupErr *containerCleanupError
		if errors.As(err, &cleanupErr) {
			e.pendingContainers = append(e.pendingContainers, cleanupErr.containerID)
		}
		return "", fmt.Errorf("failed to restart from image %s: %w", imageID, err)
	}

	// Keep the original container/config intact if preparation or removal fails.
	if err := e.removeContainer(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cleanupErr := newEngine.removeContainer(cleanupCtx)
		if cleanupErr != nil {
			e.pendingContainers = append(e.pendingContainers, newEngine.containerID)
		}
		return "", errors.Join(fmt.Errorf("failed to destroy old container during restart: %w", err), cleanupErr)
	}

	e.containerID = newEngine.containerID
	e.config = newCfg // 同步更新 config，避免 Image 字段过期
	return e.containerID, nil
}
