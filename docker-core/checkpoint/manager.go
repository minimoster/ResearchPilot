package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"github.com/yu-xin-c/Sea-mult-agent/docker-core/sandbox"
)

var checkpointSequence atomic.Uint64

var dockerRefSanitizer = regexp.MustCompile(`[^a-z0-9._-]+`)

// RollbackResult 回滚操作的结果
type RollbackResult struct {
	OldContainerID string // 被销毁的旧容器 ID
	NewContainerID string // 新拉起的容器 ID
	RestoredNodeID string // 回滚到的 DAG 节点 ID
	ImageID        string // 使用的快照镜像 ID
}

// Manager checkpoint 核心管理器
// 协调 Docker Commit / 容器重建 / 负反馈注入
type Manager struct {
	sandbox     *sandbox.Sandbox
	registry    *SnapshotRegistry
	injector    *FeedbackInjector
	mu          sync.Mutex // Serializes Commit, Rollback and owned-image cleanup.
	cli         *client.Client
	ownedImages map[string]uint64
}

// NewManager 创建 checkpoint 管理器，绑定到指定沙箱
func NewManager(sb *sandbox.Sandbox) *Manager {
	m := &Manager{
		sandbox:     sb,
		registry:    NewSnapshotRegistry(),
		injector:    NewFeedbackInjector(),
		ownedImages: make(map[string]uint64),
	}
	if sb != nil && sb.Engine() != nil {
		m.cli = sb.Engine().Client()
		sb.AddCleanup(m.Cleanup)
	}
	return m
}

// Commit 为当前容器状态创建快照
// 对齐 executor.CheckpointManager 接口，返回 ImageID
func (m *Manager) Commit(ctx context.Context, nodeID string) (string, error) {
	if m == nil || m.sandbox == nil || m.sandbox.Engine() == nil {
		return "", fmt.Errorf("no sandbox available to commit")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	engine := m.sandbox.Engine()
	containerID := engine.ContainerID()
	if containerID == "" {
		return "", fmt.Errorf("no active container to commit")
	}

	ref := fmt.Sprintf("checkpoint-%s-%d-%d", sanitizeDockerRefComponent(nodeID), time.Now().UnixNano(), checkpointSequence.Add(1))

	// Docker Commit：将当前容器文件系统 + 环境状态打包为新镜像
	resp, err := engine.Client().ContainerCommit(ctx, containerID,
		container.CommitOptions{
			Reference: ref,
			Comment:   fmt.Sprintf("Checkpoint for DAG node: %s", nodeID),
		},
	)
	if err != nil {
		return "", fmt.Errorf("docker commit failed for node %s: %w", nodeID, err)
	}

	if strings.TrimSpace(resp.ID) == "" {
		return "", fmt.Errorf("docker commit returned an empty image ID for node %s", nodeID)
	}
	m.recordSnapshot(nodeID, resp.ID, ref)

	return resp.ID, nil
}

// recordSnapshot is called only for an image returned by this manager's Commit.
// Registry edits alone never establish image ownership. Caller holds m.mu.
func (m *Manager) recordSnapshot(nodeID, imageID, ref string) {
	m.ownedImages[imageID] = checkpointSequence.Add(1)
	snap := Snapshot{
		NodeID:    nodeID,
		ImageID:   imageID,
		CreatedAt: time.Now().Unix(),
		Comment:   ref,
	}
	m.registry.Register(snap)
}

func sanitizeDockerRefComponent(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	v = dockerRefSanitizer.ReplaceAllString(v, "-")
	v = strings.Trim(v, "-._")
	if v == "" {
		return "node"
	}
	return v
}

// Rollback 基于指定镜像 ID 执行回滚
// 对齐 executor.CheckpointManager 接口
func (m *Manager) Rollback(ctx context.Context, imageID string) error {
	if m == nil || m.sandbox == nil || m.sandbox.Engine() == nil {
		return fmt.Errorf("no sandbox available to roll back")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	engine := m.sandbox.Engine()

	// 基于快照镜像重启容器
	_, err := engine.RestartFrom(ctx, imageID)
	if err != nil {
		return fmt.Errorf("rollback failed: %w", err)
	}

	// 重新建立 TTY 会话
	if err := m.sandbox.ReattachSession(ctx); err != nil {
		return fmt.Errorf("rollback succeeded but session reattach failed: %w", err)
	}

	// 查找 imageID 对应的 nodeID，清理注册表
	for _, snap := range m.registry.All() {
		if snap.ImageID == imageID {
			m.registry.RollbackTo(snap.NodeID)
			break
		}
	}

	return nil
}

// RollbackToNode 回滚到指定 DAG 节点的快照
func (m *Manager) RollbackToNode(ctx context.Context, nodeID string) error {
	snap, ok := m.registry.GetByNodeID(nodeID)
	if !ok {
		return fmt.Errorf("no snapshot found for node: %s", nodeID)
	}

	return m.Rollback(ctx, snap.ImageID)
}

// RollbackToLatest 回滚到最近一次快照
func (m *Manager) RollbackToLatest(ctx context.Context) error {
	snap, ok := m.registry.GetLatest()
	if !ok {
		return fmt.Errorf("no snapshots available for rollback")
	}
	return m.RollbackToNode(ctx, snap.NodeID)
}

// RecordFailureAndGetFeedback 记录失败并返回负反馈提示词
func (m *Manager) RecordFailureAndGetFeedback(nodeID, command, stderr string) string {
	m.injector.RecordFailure(FailureRecord{
		NodeID:    nodeID,
		Command:   command,
		Stderr:    stderr,
		Timestamp: time.Now(),
	})
	return m.injector.GenerateNegativeFeedback(command)
}

// GetFullFeedbackContext 获取完整历史失败上下文
// 应注入到 LLM system prompt 最前端
func (m *Manager) GetFullFeedbackContext() string {
	return m.injector.GenerateFullContext()
}

// Registry 暴露快照注册表（供外部查询）
func (m *Manager) Registry() *SnapshotRegistry {
	return m.registry
}

// Injector 暴露负反馈生成器
func (m *Manager) Injector() *FeedbackInjector {
	return m.injector
}

// Cleanup removes only images created by this manager, including overwritten
// and rolled-back snapshots. It never forces removal of images still in use.
// Successful removals leave the registry; failures retain ownership for retry.
// Sandbox.Close calls this after removing its containers. Explicit callers
// should likewise stop containers using the snapshots before cleanup.
func (m *Manager) Cleanup(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ownedImages) == 0 {
		return nil
	}
	if m.cli == nil {
		return fmt.Errorf("no Docker client available for checkpoint cleanup")
	}
	ids := make([]string, 0, len(m.ownedImages))
	for id := range m.ownedImages {
		ids = append(ids, id)
	}
	// Child snapshots are newer than their parent images. Remove them first.
	sort.Slice(ids, func(i, j int) bool { return m.ownedImages[ids[i]] > m.ownedImages[ids[j]] })
	var errs []error
	for _, id := range ids {
		_, err := m.cli.ImageRemove(ctx, id, image.RemoveOptions{Force: false, PruneChildren: false})
		if err != nil && !client.IsErrNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to remove checkpoint image %s: %w", id, err))
			continue
		}
		delete(m.ownedImages, id)
		m.registry.removeImage(id)
	}
	return errors.Join(errs...)
}
