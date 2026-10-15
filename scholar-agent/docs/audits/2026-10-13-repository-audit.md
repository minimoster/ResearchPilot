# agi-openresearch 整仓审查与修复记录

日期：2026-10-13 UTC。基线：`5f876faabba238563001151fc7a573581ddaf869`。
工作分支：`codex/repo-audit-20261013`。本报告记录第一轮独立云端审查结果；第一轮交付时尚未发布。后续经用户授权发布到独立分支，未 merge 或部署。

后续状态请看 [第二轮完善记录](2026-10-13-follow-up-hardening.md)。以下保留第一轮的历史结果。

## 结论与范围

本轮覆盖前端工作台、Go API/规划/调度/存储、科研 Agent、两个沙箱实现、早期 docker-core、tool 示例、可选 Python 意图服务和 CI。重点是能用离线回归证明确有影响的正确性、隔离边界、恢复与证据校验问题；没有宣称完成生产级安全认证。

原始提交的三个 Go 模块、前端 lint/build、Python 示例测试可以通过，但这些测试未覆盖本轮发现的故障路径。新增回归在原实现上失败后再修复。`tool` 原来未纳入 CI，存在语法错误，现已修复并纳入检查。

## 模块地图

- `scholar-agent/frontend`：React 19 + TypeScript/Vite 工作台、SSE 状态、PDF/对话。
- `scholar-agent/backend`：Gin API、Intent、Planner、Scheduler、PlanStore、Agent、嵌入式沙箱与单文件打包。
- `scholar-agent/docker-sandbox`：独立 Go 服务，Docker CLI/OpenSandbox 适配。
- `docker-core`：早期 Docker SDK/TTY、checkpoint、executor、watchdog。
- `tool`：标准库工具调用示例。
- `ai-services/intent_recognition`：未接入默认 Compose 的关键词 mock，不能视为训练好的分类服务。

## 已修复的主要问题

### 高优先级：安全与证据完整性

1. 沙箱 `output_plot.png` 可通过符号链接泄露宿主机文件。使用受限 `os.OpenRoot`、普通文件检查、非阻塞打开与 8MiB 上限；两引擎和 direct API 共用或同步实现。
2. Docker CLI 接受选项形态的镜像/容器 ID、解析 warning 为容器 ID。增加严格验证和统一解析。两引擎创建持久 managed label，exec/delete 前 inspect 校验归属；无 token 的独立服务默认只监听 loopback。
3. `/execute` 用调用者 task ID 拼接宿主机路径且重复使用目录。改为权限受限的随机临时目录，清理完成才删除目录。
4. PDF 代理仅挡 RFC1918，遗漏共享网络、保留/基准范围。补充 IPv4/映射地址拦截，并关闭每次代理的 idle transport。
5. 上传文件名长扩展名可触发切片 panic，中文文件名截断会破坏 UTF-8。改为有界 UTF-8 安全截断；读取文本摘要不再先载入完整文件。
6. 完整论文复现可绕过审批：治理逻辑仅看意图约束，遗漏真实执行节点输入和环境覆盖。统一运行模式判定并在 full 模式要求审批。
7. Benchmark 信任适配器自报 target、允许挑样本、丢失 index。现将逐行预测绑定到经过哈希校验的原始数据，要求连续零起始 index 和完整前 N 行；拒绝非有限/溢出指标、归一化后重名指标，保留 JSON 数值标签精度。
8. Intent 短期记忆仅按 session 分区。改为 user+session 的 v2 哈希命名空间，杜绝跨用户共享 session 的历史混用。
9. PDF.js 3.11.174 命中 GHSA-wgrm-67xf-hhpq。两个 Viewer 共用强制 `isEvalSupported: false` 的参数，采用本地同版本 worker。[Mozilla 官方缓解说明](https://github.com/mozilla/pdf.js/security/advisories/GHSA-wgrm-67xf-hhpq)。这是缓解，不是 PDF 依赖大版本升级。

### 可靠性与恢复

- 计划 deadline 返回但留下 in_progress：现持久化取消终态并失效执行租约。
- 并行批次超过剩余尝试预算：派发槽位由剩余预算约束。
- 重分配任务后下游永久 blocked：恢复受影响节点；旧失败执行不能在手动 retry 后再次 block 新执行。
- SSE 重试重放历史终态提前关流：区分当前运行终态，关闭前排空持久历史。
- 文件持久化失败已污染内存：临时快照在 rename 提交点之后才替换内存；深拷贝嵌套 maps/slices。
- Planner 遗漏纯数据/混合依赖环和自引用 artifact：统一校验。
- 流输出错误后遗留 CLI/管道/goroutine：始终关闭/等待，明确报告超长行与输出上限；清理失败不再伪报成功。
- OpenSandbox 忽略 context、HTTP 失败或空 ID：两个实现同步校验；不支持的非空挂载明确报错。客户端有界 NDJSON 上限覆盖图片 base64 和最大输出转义，避免旧 1MiB 限制误拒绝正常结果。
- docker-core 创建/attach 失败后复用已取消 context 导致清理失败：使用独立限时清理；取消信号完成后才释放 TTY 会话。
- 科研补丁使用过期源码上下文、原文件被删除后无法恢复：先核对当前源码；允许恢复删除文件，一处失败不阻断其他安全文件恢复。
- Python 导入产生字节码被误判为源码变更：benchmark 执行禁用 pyc 写入。

### 前端与工程检查

- 短断线关闭 EventSource 导致卡住：保留浏览器重连，去重重放。
- 切换会话后旧 SSE/HTTP 更新当前视图：流、请求和异步 continuation 绑定会话/计划代次。
- 取消不再覆盖已完成/失败节点；附件成功提交后只消费本次附件；loading/uploading/IME 和同步门闩阻止重复提交。
- 移除定时器模拟翻译成功，明确显示未接入。
- `tool` 修复语法、空历史与参数编码/校验。
- Makefile/CI 纳入 frontend 离线测试、docker-core、tool；依赖仅做兼容范围内的 audit fix，未使用 force。

## 兼容性注意

- 旧 benchmark adapter 若没有 index，或自行少跑样本，现在会被拒绝，需要重新生成。
- 旧 session-only Redis 历史不会自动迁移/读取，因为无法可靠判定所有者。
- 超大图片/输出、特殊 IP、非法 ID/镜像/空 query 现在明确报错。
- full 复现现在确实需要人工审批。
- 升级前没有 `org.openresearch.sandbox.managed=true` label 的旧容器会被拒绝操作，需重新创建；不会静默接管既有容器。无 token 的 standalone 改为 loopback 监听，有 token 保留原监听行为。
- PDF worker 改为本地构建产物；部署须发布完整 frontend dist。

## 尚未解决与部署边界

- 身份仍基于调用方提供的用户 ID/静态 token，未实现 OIDC/RBAC/生产多租户认证；前端尚无 API_AUTH_TOKEN 登录流程。不要将服务作为不可信多租户执行平台暴露到公网。
- Docker socket 仍是高权限能力。CLI 被取消不等于 daemon 内 exec 已终止；租约只能拒收迟到结果，不能强制终止不合作执行器。需真实 Docker 进程组取消验收。
- 计划状态与事件追加不是同一事务；AppendEvent I/O 失败可能造成事件历史缺口。文件存储每次全量重写，不适合多副本/高吞吐。
- docker-core checkpoint 镜像缺少完整生命周期回收；TTY subshell 的 cd/复杂 export 持久化语义仍需重构。
- Benchmark 目标校验防止伪造 ground truth，不证明科学有效性：适配器仍能看到标签，未实施盲测/可信评测隔离。
- Windows 交叉构建在原始 SHA 与修复版都失败，原因是 scheduler 的 Unix 专用 Setpgid/Kill/Statfs；本轮验收平台为 Linux。
- 前端 guest 登录迁移、固定 Transformer PDF 标题/打开逻辑仍是原型限制。
- npm audit：初始 40 个受影响包条目，兼容升级后 26（21 high、1 critical、4 low）；条目包含传递依赖链，不等于 26 个独立可利用漏洞。剩余主要为旧 PDF viewer/PDF.js、可选 canvas→node-pre-gyp→tar 6.2.1 及 KaTeX。PDF 已采用官方缓解，tar 属于可选原生安装依赖；本轮使用 `npm ci --ignore-scripts`。未强制跨 major 覆盖，不能称为“零漏洞”。

## 复现与验证

使用 Go 1.26.1、Node 24.19.0、npm 11.9.0、Python 3，在 Linux 云环境运行。Go 工具链来自官方 go.dev；Node 测试兼容项目 CI 的 Node20 编译路径。

```sh
cd scholar-agent/frontend
npm ci --ignore-scripts
cd ..
make lint
make test
make build
make package
cd backend && go test -race ./...
cd ../docker-sandbox && go test -race ./...
cd ../../docker-core && go test -race ./...
cd ../tool && GO111MODULE=off go test -race ./...
```

外部服务测试默认跳过；未设置 REAL_INTEGRATION_TEST/REAL_LLM_TEST，未调用付费 LLM，未运行真实 Docker/GPU/Redis/OpenSandbox 端到端或浏览器 E2E。

交付包中的 `verification-summary.json` 和 `logs/` 是最终状态的权威执行记录；`*-before.log` / `*-intermediate.log` 是故障复现，不应误解为最终通过记录。
