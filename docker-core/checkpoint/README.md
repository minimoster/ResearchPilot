# Checkpoint (容错闭环与状态回滚)

`checkpoint` 模块是系统环境配置的“安全网”与“后悔药”。由于 LLM 生成的安装指令具有不可预见性（有时甚至具有破坏性，如误删系统动态链接库），本模块采用物理级的容器快照机制结合认知级的上下文修正，构建了坚不可摧的容错闭环。

它与 `executor` 的 DAG 调度深度整合，确保无论 AI 如何“折腾”，整个环境构建过程都能步步为营，在遇到死胡同后能迅速回溯并绕过陷阱。

## ✨ 核心特性

### 1. 细粒度自动快照 (Automated State Checkpoints)
- **DAG 节点级锚点**：在 `executor` 完成任意一个核心目标节点，并且经过 Checker Agent 验证确认无误后，系统会自动触发 Checkpoint。
- **高兼容物理备份**：采用兼容性最强的 `Docker Commit` 策略，无缝抓取当前容器的完整文件系统与环境变量，生成唯一的 `imageID` 作为稳定的中间态快照。

### 2. 灾难级错误回滚 (Disaster Recovery & Rollback)
- **环境污染检测**：当某一节点下属的所有实现路径（Implementations）均告失败，或检测到致命错误（如 `glibc` 损坏、核心依赖树崩溃）时，触发回滚协议。
- **瞬时时空倒流**：先基于上一个成功节点对应的 `imageID` 创建并启动替代容器，再移除当前容器并切换会话；替代容器准备失败时保留原容器。

### 3. 闭环认知修正 (Cognitive Feedback Injection)
- **经验教训强制注入**：回滚后，不仅物理环境复原，系统的 `ContextInjector` 会拦截下发给 LLM 的历史对话。
- **避免重复踩坑**：在当前任务上下文的最前端，系统会强制写入一段带有强烈系统引导的负反馈记录（例如：*"Note: Path 'pip install x.x' failed and corrupted the env. Environment rolled back. DO NOT try this path again."*），引导模型动态调整策略，尝试全新的解决方案。

## 📦 快速开始

### 集成 Checkpoint 管理器

```go
ctx := context.Background()

sb, err := sandbox.New(ctx, sandbox.DefaultConfig())
if err != nil {
    return err
}
// Close removes the container and then all images owned by its managers.
// Handle its error; failed cleanup can be retried with Close.
defer func() {
    if err := sb.Close(); err != nil {
        log.Printf("sandbox cleanup failed: %v", err)
    }
}()

cp := checkpoint.NewManager(sb)
imageID, err := cp.Commit(ctx, "install-base")
if err != nil {
    return err
}

// A failed replacement create/start preserves the existing container.
if err := cp.Rollback(ctx, imageID); err != nil {
    return err
}
feedback := cp.RecordFailureAndGetFeedback("install-cuda", "pip install cuda-python", "install failed")
fmt.Println(feedback)
```

### Snapshot ownership and cleanup

- A manager owns only image IDs returned by its successful `Commit` calls. Manually adding a registry entry does not transfer ownership.
- Recommitting a node or rolling the registry back does not discard ownership of older images. They remain available for cleanup at the end of the sandbox lifecycle.
- `Sandbox.Close()` first removes its containers, then calls manager cleanup hooks, then closes the Docker client. Every hook is attempted even if another hook fails, and errors are combined.
- `Manager.Cleanup(ctx)` is also available explicitly. It removes owned snapshots newest-first, never uses forced image deletion, and disables parent-image pruning. In-use images remain owned and produce an error; stop their containers and retry. Successful removals (including images already missing) also remove the corresponding registry entries.
- Cleanup is retryable: successfully removed images are not deleted again. Failed removals retain their ownership records. No automatic background garbage collection or crash-persistent image inventory is provided.
- Close deletes snapshots as part of releasing sandbox resources. Export any image that must outlive this sandbox into separately managed storage before closing.
- Manager commit, rollback, and cleanup calls are serialized. Callers must stop command execution and lifecycle changes before closing the sandbox; concurrent sandbox execution/close is not supported.

The imports used above are `context`, `fmt`, `log`, and the `checkpoint` and `sandbox` packages under `github.com/yu-xin-c/Sea-mult-agent/docker-core`. The excerpt belongs in a function returning `error`.


## 🏗 架构组件
Manager: 协调 Docker Client 的 Commit 与重置逻辑，管理当前运行容器的生命周期。

SnapshotRegistry: 快照注册表，记录 DAG 节点 ID 与 Docker imageID 的映射关系，支持链式回溯。

FeedbackInjector: 上下文修正器，负责将失败的路径转化为机器可读的强烈负面提示词（Negative Prompt），阻断 LLM 的错误循环。