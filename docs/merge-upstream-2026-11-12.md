# 上游 production 合并记录（2026-11-12）

状态：上游合并与本地验证完成。真实 Redis/PostgreSQL、生产环境和真实模型效果的验证边界见下文。

## 来源与保留内容

- 当前工作分支：`production`，合并前为 `633c540ba`。
- 沿用既有上游：`https://github.com/ranxi2001/sub2api.git` 的 `production`，本次固定到 `3e07fa89096cb58b260c032aa6de2cb2a2aa98b7`。
- `origin` 为自己的 `wubowuboOne/sub2api`，拉取时没有比本地跟踪分支更新的提交。
- 上游相对共同祖先有 244 个提交，涉及 623 个文件；包含 v2.21.0、Qwen 平台/缓存/严格会话、账号缓存统计、加密 agent 历史归属策略和单 API Key 上下文上限等更新。
- 合并前备份分支：`backup/production-before-upstream-20261112-633c540ba`。
- 保留前置提示词模块及原有两项修复：按客户端原始模型选取提示词；Messages → Responses 续接每轮保留当前 system/developer 指令。提示词内容未调整。

## 冲突处理

六个冲突文件：`openai_gateway_handler.go`、`api_key_auth_cache_impl.go`、`openai_account_scheduler.go`、`openai_agent_message_recovery.go`、`openai_gateway_forward.go`、`openai_ws_agent_recovery.go`。

- 两侧均使用鉴权快照版本 34，新增字段却不同。合并后升为 35，强制重新读取旧快照，同时保留分组联网搜索账号和 API Key 上下文上限。
- 严格归属模式保留本地工具结果续接的同账号配置重试及禁止跨账号重放；上游归属放开模式保留完整历史，只允许一次完整尝试，不能借池重试再次发送。
- 保留本地停用账号解除任务归属的行为及诊断字段；归属放开模式不主动释放已有生产者记录。
- 保留上游 Qwen 严格会话约束和响应指针的账号归属检查。
- 路由和分组页面自动合并后人工复查，保留提示词、联网搜索账号字段、模型映射、上下文检查及上游新增字段。
- 额外修复前置提示词与上游请求体容量保护的兼容：读取内存不足沿用上游 503 和 Retry-After，请求取消沿用 499，避免统一误报为 400；实际注入失败仍走原有校验错误处理。

## 新增合并回归

- `TestAPIKeyAuthCacheMergedContextLimitAndWebSearch`：缓存序列化往返同时保留两侧新增字段；版本 33/34 的缓存均需重新加载。
- `TestIssue805MergedOwnerOptOutNeverUsesPoolRetry`：真实 handler 转发路径，覆盖已知/未知归属与有/无工具结果四种组合，开启归属放开后只发送一次。
- `TestGroupPromptMappingContextCeilingIncludesPrompt`：沿生产中间件顺序执行，确认按客户端别名注入，后续上限检查看到完整提示词，超限拒绝转发；足够上限则完整放行。

- `TestGroupPromptUpstreamBodyBudgetAndCancellation`：使用实际请求体预算中间件，覆盖 Messages/Responses/Gemini 的容量不足、取消和正常请求。修复前 6 个错误分支均复现误报 400，正常分支通过；修复后复测完整中间件/路由包。

## 验证

- 使用 64 位 Go 1.27.4、`-tags=unit -p 1 -count=1`，完整运行 groupprompt、apicompat、middleware、routes、handler/admin 五个包：1,643 个顶层测试通过，含子项 3,215 次通过。另有 1 个 PostgreSQL 集成测试因未配置 `SCOPED_ADMIN_TEST_DSN` 按原测试逻辑跳过。
- 前端分组/提示词/映射/定价/平台等 25 个文件共 171 项测试通过；中英文键完整性另 3 项通过。`vue-tsc -b` 与 Vite 生产构建通过。
- 提示词核心内容、HTTP 原始模型选择和 Messages 续接保留逻辑与合并前一致；打包默认提示词的 Git 内容哈希仍为 `37037f95cb2b0c7a1e0945cf93a4da533dcc9e43`，与 test 源分支一致。
- 服务层专项 279 个顶层测试通过（含子项 578 次），handler 专项 78 个顶层测试通过（含子项 362 次）。Go 的父项通过事件包含子项跳过的测试组；另有 54 个 Redis 集成用例/子项因缺少真实 `redis-server` 按原逻辑跳过，未将其作为已验证的跨节点行为。新增加的池重试合并回归四个分支全部实际执行通过。
- 迁移包 45 个顶层静态测试、含子项 126 次通过；没有执行实际数据库迁移。
- 已扫描暂存区冲突标记，无残留。`git diff --cached --check` 仅报告上游 `250_openai_cache_write_ttl.sql` 原有的文件尾空行；该文件暂存 blob 与上游完全一致，为保持迁移原始内容不作格式修改。排除此文件后的检查通过。
- 请求体预算兼容修复后，完整复跑 middleware/routes：393 个顶层测试、含子项 994 次通过，零失败/跳过；新增三类入口共 9 个场景实际通过。
- 最终代码在 Windows amd64、Linux amd64 上的 `go build -mod=readonly -p 1 -tags embed -trimpath ./cmd/server` 均通过，包含已重新构建的前端和打包默认提示词。测试二进制位于系统临时目录，未部署。

## 运行与部署边界

- 上游将 Go 要求升级到 1.27.4，Dockerfile/CI 已随上游更新；本地使用 64 位 Go 1.27.4 验证。
- 本次新增数据库迁移：249（用量索引）、250（缓存 TTL 计费字段）、340（VIP 垫付邮件）、341（API Key 上下文上限）。合并代码未连接或改动实际数据库，部署时需按项目流程执行迁移。
- 上一轮验证已记录 Windows 下服务整包的既有失败。本次专项通过不能替代全仓、真实 Redis/PostgreSQL 集成和真实模型效果验收。
- 未推送远端、部署或调用真实计费模型接口。

Agent：Codex；模型：GPT-6（完整后端模型 ID 未提供）；推理强度：未知（会话未暴露配置）；参与：合并、冲突处理、测试及本记录；无子 Agent。
