# /v1/messages 使用 OpenAI 账号联网搜索

## 配置

1. 将一个可用的 OpenAI 账号加入分组。支持 OAuth、PAT 和 API Key；API Key 可使用官方地址或第三方上游地址。
2. 在「分组 → 编辑 → Codex 网页搜索计费 → 专用网页搜索账号」选择该账号并保存。复用已有的 `web_search_account_id`，不需要数据库迁移。
3. 分组须允许 Messages 分发（`allow_messages_dispatch`）。支持 OpenAI 分组，以及目标平台解析为 OpenAI 的 Composite 分组；原生 Anthropic 分组不走这条 OpenAI 分发路径。
4. 搜索账号的实际目标模型必须支持 **Responses API 的 hosted `web_search`**。如客户端使用公开别名，在该搜索账号中配置别名到实际 OpenAI 模型的映射。支持独立 `/alpha/search` 并不等于支持 Responses 联网搜索，反之亦然。
5. 如果账号只用于搜索，可启用账号的「仅网页搜索」；「关闭网页搜索」会禁止它承接这类请求。搜索账号还须处于可调度状态。

Key 的已有上下文权限继续生效。项目将 hosted 搜索视为无法本地计数的上下文；禁止长上下文的 Key、或设置了必须本地验证的上下文上限的 Key，仍可能返回 `context_length_exceeded`。按实际授权配置 Key，系统不会因指定了搜索账号而绕过限制。

## 请求范围

仅当请求 `tools` 中恰好有一个网页搜索工具、且未指定 `tool_choice: none` 时，才使用分组的专用搜索账号。例如：

```json
{
  "model": "your-public-model",
  "max_tokens": 1024,
  "stream": true,
  "messages": [{"role": "user", "content": "搜索本周发布的相关公告，并列出来源"}],
  "tools": [{"type": "web_search_20250305", "name": "web_search"}]
}
```

实际入站路径为复数 **`/v1/messages`**，没有新增 `/v1/message` 别名。

没有指定搜索账号、没有搜索工具、携带多种工具、或明确禁止工具调用时，继续使用原有对话路由。这项配置不会为不带搜索工具的对话自动开启联网，也不会把多工具 Agent 回合整体改成独立搜索。

## 转发与响应

- 搜索通过所选账号的 `/v1/responses` 执行，发送 hosted `web_search` 和 `tool_choice: required`。API Key 使用账号配置的上游地址；OAuth/PAT 使用其既有 ChatGPT/Codex 鉴权路径。
- 保留完整的分组前置提示词、客户端 system、对话内容与模型映射。Responses 转换按映射后的模型选择兼容字段，客户端响应继续使用公开模型名。
- 多段用户文本全部保留。支持转发 `allowed_domains` 和 `user_location`；由于 hosted 工具没有 `blocked_domains` 字段，非空黑名单会明确返回 400，而不会悄悄丢弃限制。
- 返回 Anthropic Messages JSON 或 SSE，包含 `server_tool_use`、`web_search_tool_result`、来源链接和答案文本。为验证搜索是否完成，上游流会先被完整读取，再转换为客户端响应。
- 搜索成功须同时有成功结束的 Responses 流和 `status: completed` 的 `web_search_call`。错误流、截断流、仅回答但没有执行搜索均返回失败，不产生成功搜索用量。
- 沿用分组已有的搜索按次计费与费率配置；一次成功的搜索请求记一次。HTTP 错误、重试失败或转换失败不会记成功搜索用量。此处是本站的分组计费口径，上游供应商可能有自己的工具和模型费用。

账号不可用、被暂停或重试耗尽时返回搜索失败，不会换到普通对话账号。已保存的独立搜索探测失败记录只约束独立搜索路径，不会仅因 `/alpha/search` 不存在就禁止 Messages 的 Responses 搜索；下方探测按钮仍主要用于检查独立搜索，不代替这条新路径的联调。

## 本地回归覆盖

后端测试覆盖 OAuth/PAT/API Key 地址和鉴权、完整长提示词、模型映射、JSON/SSE、完整搜索事件、错误/截断响应、Key 上下文权限、专用账号调度、重试、暂停/禁用、Composite OpenAI 目标、计费与普通请求隔离。前端继续使用既有分组账号选择器，并补充了中英文说明。

测试使用本地模拟上游，不会调用真实收费接口。实际部署后仍需确认所选账号及映射模型在该上游支持 hosted `web_search`。
