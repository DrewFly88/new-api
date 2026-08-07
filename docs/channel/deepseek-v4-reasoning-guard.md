# DeepSeek V4 `reasoning_content` 完整性守护设计

> 面向 DeepSeek V4 系列模型在 thinking 模式 + 多轮 tool-calling 场景下因 `reasoning_content` 丢失触发 `The reasoning_content in the thinking mode must be passed back to the API` (HTTP 400) 的问题，在 new-api 中新增诊断、服务端回填、客户端引导三层能力的设计文档。

---

## 1. 背景与问题根源

### 1.1 报错现象

```
HTTP 400
error: The reasoning_content in the thinking mode must be passed back to the API
```

触发条件：

- DeepSeek V4 系列模型（`deepseek-v4-pro`、`deepseek-v4-flash` 等）
- Thinking 模式启用（`thinking.type = enabled` 或 `reasoning_effort != none`）
- 多轮 tool-calling 场景

### 1.2 根本原因（跨轮次状态丢失）

DeepSeek 官方要求：**在 tool-calling 场景中，每轮 assistant 消息的 `reasoning_content` 必须在后续所有轮次中回传给 API**。

但在多轮传递过程中：

```
Turn 1 response:
  assistant message:
    - tool_calls
    - reasoning_content      ← 第 1 轮响应包含

Turn 2 request:
  assistant message keeps:
    - tool_calls
  assistant message drops:
    - reasoning_content      ← 第 2 轮请求丢失

DeepSeek API:
  HTTP 400                   ← 触发报错
```

这**不是单点 bug**，而是跨轮次状态错误：许多 OpenAI-compatible 上游框架（OpenAI JS SDK、LangChain、AG-UI、CopilotKit 等）在多轮传递过程中会把 `reasoning_content`（DeepSeek provider-specific 字段）丢掉，导致第 N 轮请求缺少该字段，触发 400。

### 1.3 deepseek-compat-kit 的方案

[`xiaoshuo1988130/deepseek-compat-kit`](https://github.com/xiaoshuo1988130/deepseek-compat-kit) 提供了一个**状态化保守缓解**（非无状态魔法修复）的本地 OpenAI-compatible 中继 proxy：

1. **代理全程拦截**：整个对话从第 1 轮起必须经过本地 proxy
2. **响应捕获**：从第 1 轮响应中捕获 `tool_call_id` 对应的 `reasoning_content`，存入内存缓存（默认 TTL 1 小时，`--state-ttl-ms` 可配置）
3. **请求时保守恢复**（`DSK_REASONING_003`）：
   - 仅当某 assistant 消息的**每个** tool call 都能从同一 turn 找到缓存命中时才回填
   - 部分命中 / 混合跨 turn 命中 → 不动，原样转发，仅诊断报告
   - 避免跨 turn 或跨 session 的 reasoning 串位
4. **诊断**：`diagnose` 子命令检查 JSONL run 中是否存在 reasoning_content 丢失
5. **限制**：如果 `reasoning_content` 在进入中继前已丢，proxy 无法凭空恢复，只能诊断

---

## 2. new-api 现状审查

### 2.1 现有相关能力

| 维度 | 现状 | 位置 |
|---|---|---|
| 入站 DTO 字段 | `Message.ReasoningContent *string json:"reasoning_content,omitempty"` 已声明，能正确接收客户端回传的 reasoning_content | `relaykit/dto/openai_request.go:308` |
| 上游 URL 路径 | 按 RelayFormat 分发到 `/v1/chat/completions`、`/anthropic/v1/messages`、`/responses`，且自动加 `/beta` 后缀 | `relay/channel/deepseek/adaptor.go:59-77` |
| Thinking 后缀解析 | `ParseDeepSeekV4ThinkingSuffix` + `applyDeepSeekV4OpenAIThinkingSuffix` 已实现，能正确设置 `THINKING`/`ReasoningEffort` | `relaykit/relayconvert/reasoning/suffix.go`、`relay/channel/deepseek/adaptor.go:96-121` |
| `thinking_to_content` 渠道设置 | 已实现（出站方向把 `reasoning_content` 转成 `⇪` 标签拼接到 content） | `relaykit/dto/channel_settings.go:15`、`relay/channel/openai/adaptor.go:95` |
| 流式 reasoning_content 捕获 | 已在 OpenAI helper 等处处理 | `relay/channel/openai/helper.go`、`relay/channel/openai/relay-openai.go` |
| 阿里百炼（ali）渠道托管 deepseek-v4 | 已将 `deepseek-v4` 列入默认 Anthropic Messages 模型，走 Claude 格式 `/anthropic/v1/messages` | `relay/channel/ali/adaptor.go:30` |
| 第三方聚合渠道托管 deepseek-v4 | 用户可在 OpenAI 类型渠道、OpenRouter/SiliconFlow 等聚合渠道手动配置 `deepseek-v4-*` 模型 | `constant/channel.go`、渠道路由层 |

### 2.2 关键结论：new-api **不存在** deepseek-compat-kit 所针对的 bug

理由：

1. **入站解析保留了 `reasoning_content`**：`Message.ReasoningContent *string json:"reasoning_content,omitempty"`，客户端若正确回传，new-api 会原样保留
2. **DeepSeek adaptor 的 `ConvertOpenAIRequest`**（`adaptor.go:85-94`）只做 thinking suffix 处理，**没有丢弃 messages 数组中的任何字段**。返回的是同一个 `*dto.GeneralOpenAIRequest` 指针，其 `Messages []Message` 字段完整保留，序列化时 `reasoning_content` 会被原样 marshal 给上游
3. **出站响应**：`openai.Adaptor.DoResponse` 走标准 OpenAI 流式/非流式路径，会保留 `reasoning_content` 字段并返回给客户端

也就是说，**只要客户端遵循 DeepSeek 协议正确回传 `reasoning_content`，new-api 的 relay 链路是字段透传的，不会触发该 400**。new-api 本身不是"丢失 reasoning_content 的上游框架"。

### 2.3 相邻场景的缺口

| 场景 | new-api 现状 | 缺口 |
|---|---|---|
| **A. 客户端回传的 messages 缺失 reasoning_content**（最常见） | 无任何检测或缓解 | 客户端框架未持久化 reasoning_content，第 2 轮请求的 assistant tool-call message 缺失该字段 → DeepSeek 400。这是 deepseek-compat-kit proxy 模式真正解决的问题 |
| **B. 服务端跨请求 reasoning_content 缓存回填** | 无 | 与 deepseek-compat-kit 的 stateful 缓存对应 |
| **C. 缺失字段的诊断信息** | 无 | 400 直接透传给客户端，客户端不知道是 reasoning_content 丢失导致 |

### 2.4 适用范围（关键设计约束）

**守护逻辑的触发判定必须绑定到"模型名模糊匹配 `deepseek-v4`"，而非"渠道类型 = DeepSeek（type=43）"。**

理由：`reasoning_content` 丢失问题是**模型协议特性**（DeepSeek V4 thinking mode 的要求），与渠道类型无关。new-api 中 `deepseek-v4-*` 模型实际会经以下渠道路径触发同一个 400：

| 渠道路径 | 承载情况 | 是否触发同一 400 |
|---|---|---|
| DeepSeek 渠道（type=43） | 官方 `ModelList` 即 `deepseek-v4-flash/pro` 系列，base_url 默认 `https://api.deepseek.com` | 是 |
| 阿里百炼渠道（ali） | `defaultAliAnthropicMessagesModels` 已包含 `deepseek-v4`，走 Claude 格式 `/anthropic/v1/messages` | 是（同一上游 DeepSeek V4 协议） |
| OpenAI 渠道透传 | 用户在 OpenAI 类型渠道手动填 `deepseek-v4-*` 模型 + 指向 DeepSeek base_url | 是 |
| 第三方聚合/中转渠道 | OpenRouter、SiliconFlow 等聚合商也托管 deepseek-v4，用 OpenAI 兼容格式对外 | 是 |

**因此守护逻辑的判定位置不能放在 `relay/channel/deepseek/adaptor.go` 内**（那只覆盖渠道类型=43 的路径，会把 ali、OpenAI 透传、聚合渠道全部排除在外），而应提升到 **relay 入口层的请求分发前置阶段**，按模型名统一嗅探：

- 判定输入：入站请求的 `model` 字段（或经渠道模型映射后的 `UpstreamModelName`）
- 判定规则：**模糊匹配** `strings.Contains(strings.ToLower(modelName), "deepseek-v4")`——大小写不敏感 + 容忍前后缀（第三方供应商的 `DeepSeek-V4-Pro`、`v4-deepseek-pro`、`deepseekv4-pro` 等变体均命中）
- 判定位置：渠道 adaptor 的 `ConvertOpenAIRequest` / `ConvertClaudeRequest` 调用**之前**的共用前置阶段，对所有走 OpenAI/Claude/Responses 格式且模型名匹配的请求统一生效
- 渠道级开关：仍保留 `deepseek_reasoning_guard` / `deepseek_reasoning_cache` 字段，但**任何渠道**只要承载了 `deepseek-v4-*` 模型即生效（UI 上渠道设置项的条件渲染从"渠道类型=DeepSeek"改为"渠道路由或当前模型名模糊匹配 deepseek-v4"）

> 详见第 4.1 节判定模块归属的调整，以及第 5.4 节"Claude 格式对称处理"因此降级。

---

## 3. 总体目标

在 new-api 中新增**DeepSeek V4 reasoning_content 完整性守护**能力，覆盖三个层次：

| 层级 | 名称 | 是否有状态 | 默认开关 |
|---|---|---|---|
| **L1** | 入站 messages 诊断（检测缺失） | 无 | 默认开启（任何承载 `deepseek-v4-*` 模型的渠道） |
| **L2** | 服务端跨请求 reasoning_content 缓存回填（保守策略） | 有（进程内 + Redis） | 默认关闭（需显式开启，有状态成本） |
| **L3** | 客户端引导（400 时注入可读诊断信息） | 无 | 默认开启 |

---

## 4. 架构设计

### 4.1 模块归属（遵循 AGENTS.md 架构约束）

> **判定位置的关键调整**：守护逻辑的判定模块放在 relay 入口层、渠道 adaptor 调用**之前**的共用前置阶段，按模型名 `deepseek-v4-*` 统一嗅探，覆盖所有渠道路径（DeepSeek 渠道、ali 渠道、OpenAI 透传、聚合渠道）。不再放在 `relay/channel/deepseek/adaptor.go` 内（那只覆盖渠道类型=43）。详见第 2.4 节适用范围。

| 文件/包 | 职责 | 模块独立性约束 |
|---|---|---|
| `relay/reasoning_guard/guard.go`（新增） | L1 诊断 + 模型名嗅探入口（format-agnostic，对 OpenAI/Claude/Responses 三种 RelayFormat 统一判定） | 根模块；放在 `relay/` 入口层，不依赖任何具体渠道 adaptor |
| `relay/reasoning_guard/cache.go`（新增） | L2 跨请求 reasoning_content 缓存（进程内 + Redis 双实现） | 根模块 |
| `relay/reasoning_guard/guard_test.go`（新增） | 表驱动回归测试 | 根模块 |
| `relay/{dto,common}/` 入口层（修改） | 在请求分发前置阶段调用 `guard.DetectIfDeepSeekV4` + `guard.DetectReasoningContentGap`，把报告挂到 `RelayInfo`；在响应阶段检测 400 注入诊断（L3） | 根模块；一处接入覆盖所有渠道 |
| `relay/channel/deepseek/adaptor.go`（微改） | 移除原计划放在此处的守护逻辑；仅保留 thinking suffix 处理（其原有职责） | 根模块 |
| `relaykit/dto/channel_settings.go`（修改） | 新增渠道设置字段 | relaykit 模块独立性：**仅新增字段，不引入根模块依赖**，`cd relaykit && GOWORK=off go build ./...` 需通过 |
| `web/src/features/channels/lib/channel-form.ts`（修改） | 前端渠道表单 schema | Bun + i18n |
| `web/src/features/channels/components/drawers/channel-mutate-drawer.tsx`（修改） | 前端渠道设置 UI，条件渲染改为"渠道路由或当前模型名匹配 deepseek-v4" | Bun + i18n |
| `web/src/i18n/locales/{en,zh,zh-TW,fr,ru,ja,vi}.json`（修改） | i18n 文案 | i18next |

### 4.2 渠道级配置开关（新增 3 个）

在 `ChannelSettings` 中新增（遵循 AGENTS.md 的"业务规则在代码而非 GORM default tag"原则，默认值在 normalization 中设置）：

```go
type ChannelSettings struct {
    // ...existing fields...
    DeepseekReasoningGuard   bool   `json:"deepseek_reasoning_guard,omitempty"`
    DeepseekReasoningCache   bool   `json:"deepseek_reasoning_cache,omitempty"`
    DeepseekReasoningCacheTTL int64 `json:"deepseek_reasoning_cache_ttl_ms,omitempty"`
}
```

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `deepseek_reasoning_guard` | bool | `true`（任何承载 `deepseek-v4-*` 模型的渠道） | L1 诊断 + L3 引导总开关 |
| `deepseek_reasoning_cache` | bool | `false` | L2 服务端回填开关（需显式开启，有状态成本） |
| `deepseek_reasoning_cache_ttl_ms` | int64 | `3600000`（1 小时，对齐 deepseek-compat-kit） | 缓存 TTL |

---

## 5. 实施步骤（按依赖顺序）

### 5.1 阶段 1：L1 诊断 + L3 客户端引导（无状态，最先落地）

#### 步骤 1.1 — 诊断函数

在 `relay/reasoning_guard/guard.go`（新增）中实现。该模块位于 relay 入口层，不依赖任何具体渠道 adaptor，对 OpenAI/Claude/Responses 三种 RelayFormat 统一判定：

```go
// DetectIfDeepSeekV4 按模型名嗅探是否为 DeepSeek V4 系列。
// 判定规则：模糊匹配 strings.Contains(strings.ToLower(modelName), "deepseek-v4")
// ——大小写不敏感 + 容忍前后缀，覆盖第三方供应商的 DeepSeek-V4-Pro / v4-deepseek-pro / deepseekv4-pro 等变体。
// 这是守护逻辑的总闸门——与渠道类型无关，覆盖 DeepSeek 渠道、ali 渠道、OpenAI 透传、聚合渠道所有路径。
func DetectIfDeepSeekV4(modelName string) bool

// ReasoningGapReport 描述一个 assistant tool-call message 缺失 reasoning_content 的情况。
// format-agnostic：对 OpenAI 格式描述 Message.ReasoningContent 缺失，
// 对 Claude 格式描述 assistant message 中 thinking content block 缺失。
type ReasoningGapReport struct {
    HasTools           bool                       // 入站请求是否携带 tools
    IsDeepSeekV4       bool                       // 模型名是否匹配 deepseek-v4-*
    RelayFormat        types.RelayFormat          // 入站请求的 relay 格式（OpenAI/Claude/Responses）
    MissingMessages    []MissingReasoningMessage // 缺失 reasoning_content 的 assistant tool-call message
    AnyMissing         bool
}

type MissingReasoningMessage struct {
    MessageIndex int        // 在消息数组中的位置
    ToolCallIDs  []string   // 该 assistant message 携带的 tool_call id（用于 L2 缓存查找）
}

// DetectReasoningContentGap 检测入站请求在 DeepSeek V4 thinking + tool-calling 场景下
// 是否缺失 reasoning_content。format-agnostic，按 RelayFormat 分派具体检测逻辑。
//
// 检测条件（必须全部满足才启用检测）：
//   1. DetectIfDeepSeekV4(modelName) == true
//   2. req 携带 tools 参数
//
// 检测逻辑（按 RelayFormat 分派）：
//   - OpenAI/Responses 格式：遍历 messages，对 role=assistant 且携带 tool_calls 的消息，
//     检查 Message.ReasoningContent 是否为 nil
//   - Claude 格式：遍历 messages，对 role=assistant 且携带 tool_use block 的消息，
//     检查是否同时携带 thinking content block
//
// 返回结构化报告，供 L2 回填和 L3 引导使用。
func DetectReasoningContentGap(format types.RelayFormat, modelName string, messages any) ReasoningGapReport
```

#### 步骤 1.2 — 在 relay 入口层接入诊断（覆盖所有渠道）

在渠道 adaptor 的 `ConvertOpenAIRequest` / `ConvertClaudeRequest` / `ConvertOpenAIResponsesRequest` 调用**之前**的共用前置阶段，调用 `DetectIfDeepSeekV4` + `DetectReasoningContentGap`，并把报告挂到 `RelayInfo`（扩展字段或使用 context key），供响应阶段在 400 时注入诊断。

> 关键：判定发生在前置共用阶段，**不**在各渠道 adaptor 内部。这样 DeepSeek 渠道、ali 渠道、OpenAI 透传、聚合渠道所有路径一处覆盖。渠道 adaptor 仅保留其原有职责（如 DeepSeek adaptor 的 thinking suffix 处理）。

#### 步骤 1.3 — L3 客户端引导

当 L1 检测到缺失，且 L2 未启用或缓存未命中时，**不阻断请求**（让上游 DeepSeek 返回真实的 400，避免 new-api 误判）。但在 relay 入口层**响应阶段**（共用响应处理前置，各渠道 adaptor 的 `DoResponse` 之后）检测到 400 且 body 包含 `reasoning_content` 关键字时，**增强错误响应**：

- 注入响应头 `X-Newapi-Reasoning-Guard: missing-reasoning-content`
- 在错误 JSON 的 `error.message` 后追加：

  ```
  (hint: assistant tool-call messages must include reasoning_content when calling DeepSeek V4 in thinking mode; see DeepSeek thinking_mode docs)
  ```

- 用 i18n：`relay/reasoning_guard/messages.{en,zh}.json` 新增诊断文案

> 与步骤 1.2 同理，L3 引导也放在共用响应阶段，覆盖所有承载 `deepseek-v4-*` 的渠道路径，而非 DeepSeek adaptor 内。

#### 步骤 1.4 — 回归测试 `relay/reasoning_guard/guard_test.go`

表驱动：`{format, modelName, messages, 期望报告}` 多组用例：

| 用例 | format | modelName | 入站 messages | 期望报告 |
|---|---|---|---|---|
| OpenAI 格式 + 完整字段 | OpenAI | `deepseek-v4-pro` | assistant + tool_calls + reasoning_content | 无缺失 |
| OpenAI 格式 + 缺失字段 | OpenAI | `deepseek-v4-pro` | assistant + tool_calls + 无 reasoning_content | 报告缺失，ToolCallIDs 正确 |
| Claude 格式 + 完整字段 | Claude | `deepseek-v4-pro` | assistant + tool_use block + thinking block | 无缺失 |
| Claude 格式 + 缺失字段 | Claude | `deepseek-v4-pro` | assistant + tool_use block + 无 thinking block | 报告缺失，ToolCallIDs 正确 |
| Responses 格式 + 缺失字段 | Responses | `deepseek-v4-flash` | assistant + tool_calls + 无 reasoning_content | 报告缺失 |
| 非 DeepSeek V4 模型（应跳过） | OpenAI | `deepseek-chat` | 任意 | `IsDeepSeekV4=false`，不报告 |
| 非 DeepSeek V4 模型（ali 渠道常见） | Claude | `qwen-max` | 任意 | `IsDeepSeekV4=false`，不报告 |
| 无 tools 参数（应跳过） | OpenAI | `deepseek-v4-pro` | 任意 | `HasTools=false`，不报告 |
| 非 tool-call turn 缺失（不报告） | OpenAI | `deepseek-v4-pro` | assistant + 无 tool_calls + 无 reasoning_content | 不报告缺失（DeepSeek 文档：非 tool-call turn 的 reasoning_content 可省略） |

遵循 AGENTS.md：`testify/require` + `testify/assert`，表驱动断言真实不变式（检测准确性、跨 format 一致性），不写凑覆盖率测试、不写 fake fuzz/performance 测试。

### 5.2 阶段 2：L2 服务端跨请求回填（可选开关，需状态）

#### 步骤 2.1 — 缓存接口设计 `relay/reasoning_guard/cache.go`

```go
// ReasoningContentCache 在跨请求之间缓存 DeepSeek V4 的 reasoning_content，
// 用于在客户端丢失该字段时保守回填。
//
// 保守回填策略（对齐 deepseek-compat-kit DSK_REASONING_003）：
//   1. 该 assistant message 的每个 tool_call_id 都有缓存命中
//   2. 所有命中项来自同一 turn（通过缓存 value 中的 turn 标识校验）
//   3. 缓存未过期
//   满足全部条件才回填；否则不动，仅记录诊断日志，让请求原样转发。
type ReasoningContentCache interface {
    // Store 在收到上游响应时按 tool_call_id 存储 reasoning_content。
    // ttl 由渠道设置 deepseek_reasoning_cache_ttl_ms 控制。
    Store(ctx context.Context, channelID int, toolCallID, reasoningContent string, ttl time.Duration) error

    // Lookup 在转发请求前按 tool_call_id 批量查找缺失的 reasoning_content。
    // 返回 map[toolCallID]reasoningContent，未命中的 key 不出现在返回 map 中。
    Lookup(ctx context.Context, channelID int, toolCallIDs []string) (map[string]string, error)
}
```

**双实现：**

- **进程内实现**：`sync.Map` + 过期清理 goroutine（单实例部署适用）
- **Redis 实现**：复用 `common/redis.go`，key 设计 `newapi:dsreasoning:{channelID}:{toolCallID}`，value 为 reasoning_content，TTL 由渠道设置控制

渠道设置 `deepseek_reasoning_cache` 决定是否启用；默认 Redis 优先，回退进程内（遵循 AGENTS.md 的"Redis + in-memory cache"栈约定）。

#### 步骤 2.2 — 响应阶段捕获

在 relay 入口层共用响应阶段（各渠道 adaptor `DoResponse` 之后的统一处理点），解析上游响应的 `tool_calls[].id` 与 `reasoning_content`（OpenAI/Responses 格式）或 thinking content block（Claude 格式），调用 `Store`。

仅当渠道设置 `deepseek_reasoning_cache=true` 且 `DetectIfDeepSeekV4(modelName)` 为真时启用，避免无谓开销。覆盖所有承载 `deepseek-v4-*` 的渠道路径，不止 DeepSeek 渠道。

#### 步骤 2.3 — 请求阶段保守回填（对齐 deepseek-compat-kit DSK_REASONING_003 策略）

在 relay 入口层共用前置阶段（渠道 adaptor 调用之前），若 L1 报告存在缺失且 L2 启用：

1. 收集缺失 assistant message 的所有 `tool_call_id`
2. 调用 `Lookup` 查缓存
3. **保守条件**（必须全部满足才回填，对齐 deepseek-compat-kit）：
   1. 该 assistant message 的**每个** `tool_call_id` 都有缓存命中
   2. 所有命中项来自**同一 turn**（通过缓存 value 中的 turn 标识校验）
   3. 缓存未过期
4. 满足条件 → 把缓存的 `reasoning_content` 写入 `Message.ReasoningContent`（OpenAI/Responses 格式）或注入 thinking content block（Claude 格式）
5. 不满足 → 不动，仅记录 `logger.LogInfo` 诊断日志，让请求原样转发（上游会返回 400，触发 L3 引导）

> 关键：回填发生在共用前置阶段，对 OpenAI/Claude/Responses 三种格式统一处理，覆盖所有渠道路径。Claude 格式的 thinking block 注入因此与本阶段天然归一，不再需要单独的"阶段 4 Claude 对称处理"。

#### 步骤 2.4 — 跨实例一致性说明（文档，非代码）

在 `docs/channel/other_setting.md` 新增 `deepseek_reasoning_cache` 说明，明确：

- 多实例部署需启用 Redis（否则各实例缓存独立，命中率下降）
- TTL 应 ≥ 客户端多轮对话最大跨度
- 此功能为迁移期缓解，长期应推动客户端正确持久化 `reasoning_content`

### 5.3 阶段 3：前端渠道设置 UI

#### 步骤 3.1 — `web/src/features/channels/lib/channel-form.ts`

- 在 `channelFormSchema` 新增三个字段（`z.boolean().optional()` / `z.number().optional()`）
- 在 `defaultValues` 和 `parseChannel` 的 `extraSettings` 中加入默认值
- 在 `buildSettingJSON` 中序列化到 setting JSON

#### 步骤 3.2 — `web/src/features/channels/components/drawers/channel-mutate-drawer.tsx`

- 在"额外设置"区域（紧邻 `thinking_to_content` 表单项，约 line 4097-4110）新增三个 `FormField`
- **条件渲染**：当渠道路由或当前模型名模糊匹配 `deepseek-v4`（大小写不敏感 + 容忍前后缀）时显示（覆盖 DeepSeek 渠道、ali 渠道、OpenAI 透传、聚合渠道等所有承载 deepseek-v4 的路径；不再仅按渠道类型=DeepSeek 判断，详见第 2.4 节适用范围）
- 字符串文案通过 i18n key（English source string），由 `bun run i18n:sync` 同步到各语言

#### 步骤 3.3 — i18n 文案

在 `web/src/i18n/locales/{en,zh,zh-TW,fr,ru,ja,vi}.json` 新增 3 个 key（渠道设置 label + description），英文为 source，其他语言按现有风格翻译。

示例（`en.json`）：

```json
{
  "DeepSeek V4 reasoning_content guard": "DeepSeek V4 reasoning_content guard",
  "Detect and diagnose missing reasoning_content in DeepSeek V4 thinking mode + multi-turn tool-calling scenarios": "Detect and diagnose missing reasoning_content in DeepSeek V4 thinking mode + multi-turn tool-calling scenarios",
  "DeepSeek V4 reasoning_content cache (server-side backfill)": "DeepSeek V4 reasoning_content cache (server-side backfill)",
  "Cache reasoning_content across requests and conservatively backfill missing fields (requires multi-turn traffic to flow through this gateway from turn one)": "Cache reasoning_content across requests and conservatively backfill missing fields (requires multi-turn traffic to flow through this gateway from turn one)"
}
```

### 5.4 阶段 4：Claude 格式对称处理

### 5.4 阶段 4：Claude 格式对称处理（已天然归一，不再作为独立阶段）

> **降级说明**：原计划中"Claude 格式对称处理"作为独立阶段 4，是因为守护逻辑当时放在 DeepSeek adaptor 内，需单独处理 ali 渠道的 Claude 格式路径。修订后守护逻辑已提升到 relay 入口层共用前置/响应阶段（第 4.1 节），`DetectReasoningContentGap` 本身即 format-agnostic（步骤 1.1），L2 缓存回填在步骤 2.3 也已对 Claude 格式的 thinking content block 注入天然归一。**因此阶段 4 不再作为独立阶段存在。**

Claude 格式下 `reasoning_content` 对应 assistant message 中的 thinking content block，由步骤 1.1 的 `DetectReasoningContentGap` 按 RelayFormat 分派检测、步骤 2.3 的保守回填按格式注入 thinking block。ali 渠道（`relay/channel/ali/adaptor.go:30` 已含 `deepseek-v4`）、DeepSeek 渠道的 Claude 路径、任何其他承载 `deepseek-v4-*` 的 Claude 格式渠道路径，**无需额外代码**即可被守护逻辑覆盖。

落地时仅需在步骤 1.4 的回归测试中覆盖 Claude 格式用例（已在表中列出），验证 thinking block 缺失检测与回填注入的正确性。

---

## 6. 数据流示意

### 6.1 L1 诊断 + L3 引导（无状态路径）

```
Client ──POST /v1/chat/completions──> new-api
                                         │
                                         ▼
                          [relay 入口层共用前置阶段]
                          ├─ DetectIfDeepSeekV4(modelName)  ← 模型名嗅探，与渠道类型无关
                          └─ DetectReasoningContentGap(format, modelName, messages)
                              │
                  ┌───────────┴───────────┐
                  ▼                       ▼
            无缺失                    有缺失（L1 报告）
            原样转发                  原样转发（不阻断）
                  │                       │
                  ▼                       ▼
              [渠道 adaptor]          [渠道 adaptor]
              (DeepSeek/ali/OpenAI    (DeepSeek/ali/OpenAI
               透传/聚合等任一)         透传/聚合等任一)
                  │                       │
                  ▼                       ▼
                                  upstream DeepSeek V4
                                  返回 HTTP 400
                                  "reasoning_content must be passed back"
                                         │
                                         ▼
                          [relay 入口层共用响应阶段]
                          检测到 400 + body 含 "reasoning_content"
                          ├─ 注入响应头 X-Newapi-Reasoning-Guard
                          └─ 追加 hint 到 error.message（L3 引导）
                                         │
                                         ▼
                                    Client
                          收到可读诊断信息
```

### 6.2 L2 服务端回填（有状态路径）

```
Turn 1:
  Client ──POST──> new-api ──> 渠道 adaptor ──> upstream DeepSeek V4 ──> response
                                                  │                │
                                                  │                └─ reasoning_content + tool_calls
                                                  │
                                                  └─ [relay 入口层共用响应阶段]
                                                     解析 tool_calls[].id + reasoning_content
                                                     （OpenAI/Responses 格式）或 thinking block（Claude 格式）
                                                     调用 cache.Store(channelID, toolCallID, rc, ttl)
                                                     （仅当 deepseek_reasoning_cache=true 且 DetectIfDeepSeekV4(modelName)=true）

Turn 2:
  Client ──POST──> new-api
                    │
                    │ req.Messages 中 assistant tool-call message 缺失 reasoning_content
                    ▼
                 [relay 入口层共用前置阶段]
                 ├─ DetectIfDeepSeekV4(modelName) → true
                 ├─ DetectReasoningContentGap → 报告缺失
                 └─ cache.Lookup(channelID, missingToolCallIDs)
                     │
            ┌────────┴────────┐
            ▼                 ▼
        全部命中            部分命中/未命中
        保守回填            不动，原样转发
        写入 Message.ReasoningContent
        （OpenAI/Responses）或注入 thinking block（Claude）
            │                 │
            ▼                 ▼
        [渠道 adaptor]    [渠道 adaptor]
        (任一承载          (任一承载
         deepseek-v4-*      deepseek-v4-*
         的渠道)            的渠道)
            │                 │
            ▼                 ▼
        upstream          upstream
        DeepSeek V4       DeepSeek V4
        正常 200          返回 400
                         触发 L3 引导
```

---

## 7. 验证策略

| 验证项 | 命令 | 通过标准 |
|---|---|---|
| 根模块编译 | `go build ./...` | 无错误 |
| relaykit 独立编译 | `cd relaykit && GOWORK=off go build ./...` | 无错误（AGENTS.md 强制要求） |
| 新增测试 | `go test ./relay/reasoning_guard/...` | 全部通过，覆盖 L1 诊断准确性（跨 OpenAI/Claude/Responses 三种 format）、L2 保守回填条件、模型名嗅探边界 |
| 数据库兼容 | （本计划不涉及 DB schema 变更，渠道设置存 setting JSON） | N/A |
| 前端构建 | `cd web && bun run build` | 无 TS 错误，无 i18n key 缺失 |
| 前端 i18n 同步 | `cd web && bun run i18n:sync` | 所有语言文件 key 对齐 |

---

## 8. AGENTS.md 合规性自查清单

- [x] **relaykit 模块独立性**：仅新增 `ChannelSettings` 字段，不引入根模块依赖；变更后需 `cd relaykit && GOWORK=off go build ./...` 通过
- [x] **JSON 包装**：所有 marshal/unmarshal 用 `common.Marshal` / `common.Unmarshal`，不直接 `encoding/json`
- [x] **数据库兼容**：不涉及 DB schema 变更（渠道设置存 setting JSON 字段），三种数据库无差异
- [x] **计费安全不变式**：本计划不触及 quota/billing 链路
- [x] **后端测试质量**：用 `testify/require` + `assert`，表驱动断言真实不变式（检测准确性、保守回填条件），不写凑覆盖率测试、不写 fake fuzz/performance 测试
- [x] **国际化**：前端用 i18next + `t('English key')`，后端用 go-i18n；新增文案由 `bun run i18n:sync` 同步
- [x] **项目治理**：保留所有 nеw-аρi / QuаntumΝоuѕ 标识；PR 用 `.github/PULL_REQUEST_TEMPLATE.md`；如当前 git user 非历史核心开发者，PR body 需声明 AI-generated/AI-assisted

---

## 9. 文档更新

| 文件 | 更新内容 |
|---|---|
| `docs/channel/other_setting.md` | 新增 3 个渠道设置项的说明、适用场景、多实例部署注意事项；明确**适用范围为任何承载 `deepseek-v4-*` 模型的渠道**（不止 DeepSeek 渠道类型=43），见本文第 2.4 节 |
| `docs/channel/deepseek.md`（若存在）或新建 | DeepSeek V4 thinking mode + tool-calling 最佳实践，引用 DeepSeek 官方 thinking_mode 文档，说明 new-api 的诊断/回填能力边界与适用渠道路径 |
| 本文档 `docs/channel/deepseek-v4-reasoning-guard.md` | 完整设计文档 |

---

## 10. 工作量估算

| 阶段 | 文件变更数 | 估算工时 |
|---|---|---|
| 阶段 1（L1+L3） | 3 新增 + 2 修改（含 `relay/reasoning_guard/guard.go` + `guard_test.go` + 入口层接入） | 6-8 小时 |
| 阶段 2（L2 缓存） | 1 新增 + 2 修改（含 `relay/reasoning_guard/cache.go` + 入口层响应/前置接入） | 8-10 小时 |
| 阶段 3（前端 UI） | 2 修改 + 7 i18n | 3-4 小时 |
| 阶段 4（Claude 对称） | 已天然归一，不再作为独立阶段（见第 5.4 节） | 0 |
| 文档 | 2-3 修改/新增 | 1-2 小时 |
| **合计** | ~18 文件 | **18-24 小时** |

---

## 11. 参考

- **deepseek-compat-kit**: <https://github.com/xiaoshuo1988130/deepseek-compat-kit>
  - 状态化保守缓解 proxy（DSK_REASONING_003）
  - 1 小时默认 reasoning cache TTL
  - 仅当每个 tool call 都有同一 turn 的缓存命中才回填
- **DeepSeek 官方文档**:
  - [Thinking Mode](https://api-docs.deepseek.com/guides/thinking_mode/)
  - [thinking_mode_api_example_tool_call](https://api-docs.deepseek.com/api_samples/thinking_mode_api_example_tool_call)
  - 关键要求：对于携带 `tools` 参数的请求，`reasoning_content` 必须在所有后续请求中完整回传给 API，否则返回 400
- **掘金技术文章**: [DeepSeek V4 多轮 Tool-Calling 报 reasoning_content 400？原因、排查与本地中继方案](https://juejin.cn/post/7643478523496267816)
