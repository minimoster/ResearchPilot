# 第二轮完善：跨平台、事件事务与清理恢复

日期：2026-10-13 UTC。工作分支：`codex/repo-audit-20261013`。

第一轮已上传提交：`6656bba213d4f6ea3795e03493c95dc1860b04ce`；[该提交的 GitHub CI 已通过](https://github.com/minimoster/agi-openresearch/actions/runs/38344395541)。本文件记录在其上的第二轮改动；第一轮检查结果不能替代第二轮验证。main 未合并，未部署。

## 已完成

### 1. Windows 编译与平台隔离

将仓库 clone 的进程取消和磁盘空间探测拆为带 build tags 的小型平台适配函数。

- Linux/Unix 保留进程组终止；支持的 Unix 平台仍使用 Statfs。
- Windows 保留 CommandContext 对直接进程的取消，WaitDelay 限制继承管道等待。
- 无受支持磁盘 API 的平台返回未知容量零值，不能误满足 full reproduction 的默认磁盘门槛。
- Linux race 回归、Windows amd64 和 Darwin arm64 的 API/app 交叉构建通过；Windows scheduler 测试也可交叉编译。

这解决的是构建阻塞，不宣称 Windows/Darwin 运行时已验收。Windows 尚无完整后代进程树终止；未知磁盘空间会保守限制 full 模式。

### 2. 状态与事件原子提交

PlanStore 新增 `UpdatePlanWithEvents`，内存和文件实现都在同一更新中提交状态与完整事件批次。Scheduler 所有已有状态转换接入该接口，仅在提交成功后向实时总线发布。

新增故障测试覆盖：更新回调失败、JSON编码失败、rename失败、重开文件后的状态/历史一致性、并发提交、多类调度状态转换失败，以及发布前完整事件批次已经持久化。

独立日志/过期结果事件仍单独追加；追加失败不发布，并记录服务端错误。实时总线仍为 best effort，历史重放为恢复依据，不保证 exactly-once 或实时顺序。API 创建、审批和重启恢复原本没有事件，本轮未扩展审计语义。rename 后目录 fsync 失败可能表示已提交但返回错误；内存、文件中的状态与事件仍一致。

### 3. 依赖风险降低

- KaTeX 升至已修复的 0.18.2，Markdown 适配器与样式使用同一版本；补充真实 React Markdown 渲染及原型污染攻击回归。
- 项目 `.npmrc` 默认 `ignore-scripts=true`，避免运行浏览器不需要的可选 canvas 原生安装器；显式 npm test/build 不受影响。
- npm audit 受影响包条目从首轮末的26降到22（21 high、1 critical）；这包含传递依赖，不是独立漏洞数量。
- PDF viewer 的 peer 契约只支持 PDF.js 2/3。本轮不强行覆盖成不兼容的 PDF.js 4+，保留 Mozilla 官方 `isEvalSupported:false` 缓解及同版本本地 worker。

完整理由、来源及迁移边界见 [前端依赖安全说明](../../frontend/SECURITY_DEPENDENCIES.md)。旧 PDF.js/viewer 与可选 canvas→tar 链仍有扫描告警，不能宣称全部修复。

### 4. 回滚与快照回收

- Engine.RestartFrom 先准备并启动替代容器，再移除原容器；准备失败保留原状态。
- 原容器移除失败时，使用独立限时 context 回收替代容器；回收失败保留记录供 Destroy 重试。已不存在的容器视为已移除。
- Manager 单独记录每次成功 Commit 创建的镜像，注册表覆盖/rollback 截断不再丢失回收责任。
- 通用 Sandbox cleanup hooks 在容器移除后、client关闭前运行；逐项收集错误，一处失败不会跳过其他 hook。
- 镜像只按本 Manager 确认创建的 ID 回收，按新到旧顺序，不 force，不自动裁剪父镜像。失败项保留，可显式 Cleanup 或再次 Close 重试。

测试使用进程内 fake Docker API，包括 attach 连接路径，未运行真实 Docker。调用者必须先停止活跃 Execute/Commit/Rollback，再关闭沙箱；本轮不新增整个生命周期的并发 Close 保证。镜像 ownership 仍为进程内记录，进程崩溃后的持久垃圾回收不在本轮范围。

## 验证与发布

- make test、lint、build、package
- 全 backend 与 docker-core race，相关模块 vet
- Windows amd64、Darwin arm64 API/app 交叉构建
- 前端22项离线测试，包含真实 Markdown 渲染
- GitHub CI 新增 backend race 与 Windows 交叉构建检查

实际执行记录见交付包的 phase2 日志。没有启用真实Docker/GPU、付费LLM、向外部服务传送研究数据或公网部署。
