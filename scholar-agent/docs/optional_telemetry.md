# 可关闭的任务与模型 HTTP 追踪

本轮在现有任务执行链路旁增加可观测性。默认 `RESEARCHPILOT_TRACE_ENABLED=false`，不开 worker、不发送遥测请求，Eino 继续使用原来的 HTTP client。打开后也不改变模型、Prompt、任务调度、重试次数、沙箱和断点恢复逻辑。

## 已接入的边界

1. `Scheduler.runReadyNodes` 为每次 task attempt 创建 `task.execute` 父 span，携带已有 `trace_id`、`plan_id`、`task_id`、`execution_id` 和 SSE 使用的 `task_<task_id>` 关联标识。
2. Chat、Coder、Data、Librarian 四个 Agent 的既有 Eino OpenAI 客户端使用可选 HTTP transport，产生 `llm.http` 子 span。通过同一个客户端发出的 Generate 请求均可观察，包括 Coder 的修复调用。
3. 导出调用起止时间、HTTP 状态、允许的错误类别、已配置的模型名，以及提供方实际返回的 token 字段。任务成功/失败与 HTTP 成功/失败分别记录。

当前层级是同一个 plan trace 下的多个 task-attempt 根 span 和各自的 LLM 子 span，不含总 plan 根 span，也不是完整 DAG 因果树。`researchpilot.task.status` 表示该次执行返回时的状态；终结追踪发生在 store 接受结果之前，迟到结果仍可能被原有 lease 检查拒收。是否正式进入计划状态应结合既有业务事件与 plan snapshot 判断。

例如：提供方 HTTP 200 但返回空 `choices`，HTTP span 可以成功，而 Eino/父任务失败；不能把 HTTP 200 当作 Agent 任务成功。

这不是完整的全链路埋点：意图识别、Planner 模型、检索、每个工具/沙箱内部步骤暂未接入；直接调用四个 Agent 而不经过 Scheduler 时，模型请求可以形成独立 trace，缺少 task execution 关联。SSE 响应流与模型 provider 流式 API 也不是同一概念。

## token 的定义与未知值

| 提供方 JSON 字段 | OTLP 属性 | 口径 |
| --- | --- | --- |
| `usage.prompt_tokens` | `gen_ai.usage.input_tokens` | 输入 token，保持提供方的值 |
| `usage.completion_tokens` | `gen_ai.usage.output_tokens` | 输出 token，保持提供方的值 |
| `usage.total_tokens` | `gen_ai.usage.total_tokens` | 总 token，保持提供方的值，不根据缺失分量补算 |

每个字段用指针解码，**字段省略/null 与明确的 0 区分**。没有 `usage`、字段缺失、负值、响应不完整或解析失败时，不导出对应数值，也不以字符串长度估算 token。

当前 Eino 依赖的 OpenAI ACL 非流式适配器会将缺省 `usage` 转成全零结构，因此这里只在原始 HTTP 响应层判断字段是否实际存在，避免把“未知”记成“0”。

响应读取保持原来的字节、Read 错误和 Close 错误。为了提取 usage，只在内存中保留最多 1 MiB 的 JSON 响应副本，解析后释放；超过上限继续正常向模型客户端传输全部字节，但省略 usage。`json.Decoder` 读到完整 JSON 后可能在 EOF 前 Close，完整且有效的已读取 JSON 仍可提取 usage。

`researchpilot.usage.status` 会标明 `provider_reported`、`not_reported`、`invalid_or_incomplete_json`、`response_too_large`、`not_json` 或其他原因。`text/event-stream` 本轮仅记录请求生命周期，标记 `streaming_not_collected`，不统计流式 chunk usage，也不做流内业务错误判定。非 JSON、HTTP 错误或读取失败的响应不计 token。

不计算或承诺费用；不拆分 cached/reasoning token，避免重复计算。成本、成功率和质量提升需要另行做固定数据集实验。

## 启用与关闭

把下列变量配置到**启动后端进程实际使用的环境中**。只编辑示例文件不会自动修改正在运行的进程。现有启动流程如果加载 `backend.env`，可以从 `backend.env.example` 复制新变量。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RESEARCHPILOT_TRACE_ENABLED` | `false` | 显式设置 `true` 才开启 |
| `RESEARCHPILOT_OTLP_TRACES_ENDPOINT` | 空 | 完整 traces URL，包含 `/v1/traces`；不自动选择远端服务 |
| `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` | 空 | 使用 Langfuse 时成对设置；本地无鉴权 OTLP mock 可都留空 |
| `RESEARCHPILOT_TRACE_QUEUE_SIZE` | `256` | 待导出 span 队列容量，允许 1–4096 |
| `RESEARCHPILOT_TRACE_TIMEOUT` | `2s` | 单次导出超时，允许 1ms–10s；不改变模型请求超时 |

本地 Langfuse 的示例 endpoint：`http://localhost:3000/api/public/otel/v1/traces`。通用 OTLP 接收器通常使用 `http://localhost:4318/v1/traces`。使用托管服务时填写该服务的确切区域 endpoint 和自己的项目密钥；密钥放进运行环境，不能提交到仓库。

实现使用 Go 标准库发送 **OTLP/HTTP JSON**，没有引入 OpenTelemetry Go SDK。请求使用 `Content-Type: application/json`，Langfuse Basic Auth 和 `x-langfuse-ingestion-version: 4`。只支持这一导出路径，不支持 gRPC。URL 重定向不会携带鉴权跟随。

环境变量在进程内只读一次。启用、关闭或换 endpoint 后需要重启后端；关闭操作：

```sh
export RESEARCHPILOT_TRACE_ENABLED=false
# 按现有启动方式重启后端
```

配置不合法时仅关闭追踪并输出不含配置值的提示，后端仍可启动。关闭后保留已经存在的 Langfuse 记录，不产生新记录。

## 运行失败时的行为

- span 结束时只尝试入队；队列满就丢弃遥测，不等待网络，不中断任务。
- 独立 worker 使用自己的 context 发送请求；任务取消后仍可报告失败/取消的最终状态。
- 本轮采用单次发送，无持久化、无自动重试。网络失败、超时、401/503、非正常确认和 OTLP `partialSuccess.rejectedSpans` 非零或无效均计入 Failed，不算导出成功。
- 内部 `Exporter.Stats()` 区分 Enqueued、Exported、Dropped、Failed；Dropped 包含队列满、关闭后提交和关闭时剩余队列，Failed 表示已尝试但未被完整确认。首次和每 100 次导出失败会输出只含计数的诊断。
- Close 可以与 span 结束并发，截止时间到达时取消发送，不无限等待。
- 两个 main 入口只在函数正常返回时 defer 一个最多 3 秒的 drain；现有 SIGTERM/中断默认退出、`log.Fatal` 和进程强制终止路径不保证执行 defer。尚未改造服务信号关闭流程，进程退出时可能丢失尚未发送的 span。

OTLP `traceId` 使用 32 位十六进制，`spanId` 使用 16 位十六进制。业务 `trace_id` 如果不符合格式会做确定性映射；原业务标识仍留在允许的 metadata 字段里，不修改持久化计划或 SSE 协议。每次 attempt 有不同 span ID 和 execution ID，同一 attempt 的模型请求继承父 span。

## 默认导出的内容边界

仅导出上述运行标识、模型配置名、类型、时间、HTTP 状态、token 和预定义错误类别。不会导出 Prompt、回答、代码、文档内容、任意 task metadata、HTTP headers、模型 API key、provider URL 或原始 error 文本。HTTP transport 不读取请求 body，也不把内部 trace headers 注入模型提供方请求。

现有应用日志与业务事件由原有代码管理，本功能不改变它们的记录规则。这里的内容限制是新导出通道的边界。

## 无付费凭据的验证

在 `scholar-agent/backend` 中运行：

```sh
go test ./internal/telemetry
go test ./internal/scheduler -run TestSchedulerTelemetryWithRealModelClient -count=1
go test -race ./internal/telemetry
go test ./...
```

测试使用本地 `httptest` 模型提供方与 OTLP 接收端，不需要真实模型、Langfuse 账号、Redis 或 Docker。首次取得 Go 工具链和项目依赖仍可能联网。

覆盖：默认关闭保持原对象；父子 ID/时间/attempt 关联；显式 0 与缺失 usage；部分、截断、非 JSON、过大和流式响应；原 Read/Close 错误透传；导出 401/503/部分拒收；队列满和并发关闭；真实 Scheduler→Eino Generate→HTTP 的失败重试；HTTP 200 与业务失败的区别。

这些测试验证协议形状和本地应用行为。尚未配置真实 Langfuse endpoint/key 的环境不能据此宣称已在 Langfuse UI 完成联调，也不能宣称任务质量或成本已改善。

## 回退

1. 运行回退：设置 `RESEARCHPILOT_TRACE_ENABLED=false`，重启后端，恢复原模型 HTTP 路径和无导出状态。
2. 代码回退：当前功能位于独立分支，先保存未提交的修改，再切换到 `backup/pre-agent-upgrade-20261026` 即可回到升级前代码。未来合并后，按实际合并方式回退整组功能变更（包含相关 CI 引用），保留其他后续提交，再按现有流程构建部署。不要只回退最初源码提交而留下仍引用 telemetry 包的 CI 步骤。
3. 无数据库迁移、无计划文件格式变更、无新增第三方依赖。无需删除现有 plan snapshot、执行产物或用户数据。

参考协议：[Langfuse OTLP](https://langfuse.com/integrations/native/opentelemetry)、[OTLP JSON](https://opentelemetry.io/docs/specs/otlp/)、[Eino OpenAI v0.1.10](https://github.com/cloudwego/eino-ext/blob/components/model/openai/v0.1.10/components/model/openai/chatmodel.go)、[ACL OpenAI v0.1.14](https://github.com/cloudwego/eino-ext/blob/libs/acl/openai/v0.1.14/libs/acl/openai/chat_model.go)。
