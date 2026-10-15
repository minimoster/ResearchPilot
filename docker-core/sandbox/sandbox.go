package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ExecResult 单次命令执行的返回结果
type ExecResult struct {
	RawOutput        string // 完整原始输出（推理轨道原文）
	InferenceSummary string // 经 Truncator 精简后的推理摘要
	ExitCode         int    // 退出码，0 = 成功
}

// Sandbox 对外暴露的核心对象
// 封装 Engine（生命周期）、Session（TTY 会话）、Truncator（日志瘦身）
type Sandbox struct {
	engine    *Engine
	session   *Session
	truncator *Truncator
	config    Config
	cleanupMu sync.Mutex
	cleanups  []func(context.Context) error
}

// New 创建并启动一个完整的沙箱实例
func New(ctx context.Context, cfg Config) (*Sandbox, error) {
	// 1. 创建容器
	engine, err := NewEngine(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox engine: %w", err)
	}

	// 2. 建立 TTY 会话
	session, err := NewSession(ctx, engine, cfg.AuditLogDir, cfg.ExecTimeout)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cleanupErr := engine.Destroy(cleanupCtx); cleanupErr != nil {
			return nil, fmt.Errorf("failed to create sandbox session: %w; cleanup failed: %v", err, cleanupErr)
		}
		return nil, fmt.Errorf("failed to create sandbox session: %w", err)
	}

	return &Sandbox{
		engine:    engine,
		session:   session,
		truncator: NewTruncator(cfg.TruncateN, cfg.TruncateM),
		config:    cfg,
	}, nil
}

// Execute 在沙箱内执行命令，仅返回原始输出字符串
// 特别说明：为了兼容 executor.Sandbox 接口，如果 exit code != 0，将返回错误
func (s *Sandbox) Execute(ctx context.Context, command string) (string, error) {
	if s.session == nil {
		return "", fmt.Errorf("no active sandbox session")
	}
	result, err := s.session.Execute(ctx, command)
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return result.RawOutput, fmt.Errorf("command failed with exit code %d", result.ExitCode)
	}
	return result.RawOutput, nil
}

// Engine 暴露底层 Engine（供 checkpoint 模块调用）
func (s *Sandbox) Engine() *Engine {
	return s.engine
}

// AddCleanup registers dependent resource cleanup. Hooks run after containers
// have been removed and before the Docker client closes. A hook may be called
// again by a later Close, so it must retain failed work and be retryable.
func (s *Sandbox) AddCleanup(cleanup func(context.Context) error) {
	if cleanup == nil {
		return
	}
	s.cleanupMu.Lock()
	s.cleanups = append(s.cleanups, cleanup)
	s.cleanupMu.Unlock()
}

// Close 优雅关闭沙箱，释放所有资源。Cleanup failures can be retried by Close.
// Stop command execution and other lifecycle calls before calling Close.
func (s *Sandbox) Close() error {
	var errs []error
	if s.session != nil {
		errs = append(errs, s.session.Close())
		s.session = nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s.engine != nil {
		if err := s.engine.removeContainers(cleanupCtx); err != nil {
			// Images may still be in use; defer dependent cleanup until retry.
			return errors.Join(append(errs, err, s.engine.closeClient())...)
		}
	}
	s.cleanupMu.Lock()
	cleanups := append([]func(context.Context) error(nil), s.cleanups...)
	s.cleanupMu.Unlock()
	for _, cleanup := range cleanups {
		errs = append(errs, cleanup(cleanupCtx))
	}
	if s.engine != nil {
		errs = append(errs, s.engine.closeClient())
	}
	return errors.Join(errs...)
}

// ReattachSession 在回滚重建容器后重新建立 TTY 会话
// 由 checkpoint.Manager 在 Rollback 完成后调用
func (s *Sandbox) ReattachSession(ctx context.Context) error {
	// 关闭旧会话
	if s.session != nil {
		s.session.Close()
		s.session = nil
	}

	// 用新容器建立新会话
	session, err := NewSession(ctx, s.engine, s.config.AuditLogDir, s.config.ExecTimeout)
	if err != nil {
		return fmt.Errorf("failed to reattach session: %w", err)
	}
	s.session = session
	return nil
}
