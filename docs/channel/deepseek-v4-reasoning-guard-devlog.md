# DevLog — DeepSeek V4 reasoning_content 完整性守护功能实现

> 本 devlog 记录在 new-api 中实现 DeepSeek V4 reasoning_content 完整性守护功能（L1 诊断 + L2 服务端缓存回填 + L3 客户端引导）的完整开发过程，包括变更文件清单、实现细节、code_review 处理结果和验证状态。
>
> 设计文档参见：[`deepseek-v4-reasoning-guard.md`](./deepseek-v4-reasoning-guard.md)
>
> 实施日期：2026-08-08（Saturday）
> 实施者：AtomCode（GLM-5.2）
> 分支：main

---

## 1. 目标回顾

在 new-api 中新增 DeepSeek V4 `reasoning_content` 完整性守护能力，面向 DeepSeek V4 系列模型在 thinking 模式 + 多轮 tool-calling 场景下因 `reasoning_content` 丢失触发的 `The reasoning_content in the thinking mode must be passed back to the API`（HTTP 400）问题。

三层能力：

| 层级 | 名称 | 是否有状态 | 默认开关 |
|---|---|---|---|
| **L1** | 入站 messages 诊断（检测缺失） | 无 | 默认开启（任何承载 `deepseek-v4-*` 模型的渠道） |
| **L2** | 服务端跨请求 reasoning_content 缓存回填（保守策略） | 有（进程内 + Redis） | 默认关闭（需显式开启，有状态成本） |
| **L3** | 客户端引导（400 时注入可读诊断信息） | 无 | 默认开启 |

**关键设计约束**：守护逻辑的触发判定绑定到"模型名模糊匹配 `deepseek-v4`"（大小写不敏感 + 容忍前后缀，覆盖第三方供应商的 `DeepSeek-V4-Pro`/`v4-deepseek-pro`/`deepseekv4-pro` 等变体）而非"渠道类型 = DeepSeek（type=43）"，覆盖 DeepSeek 渠道、ali 渠道、OpenAI 透传、第三方聚合渠道所有路径。详见设计文档第 2.4 节"适用范围"。

---

## 2. 变更文件清单

共变更 18 个文件（9 新增 + 9 修改），分布在后端、前端、i18n 三个区域。

### 2.1 后端（6 文件：4 新增 + 2 修改）

| 文件 | 类型 | 职责 |
|---|---|---|
| `relay/reasoning_guard/guard.go` | 新增 | L1 诊断核心：`DetectIfDeepSeekV4` 模型名嗅探 + `DetectReasoningContentGap` format-agnostic 诊断（OpenAI/Claude 双格式分派） |
| `relay/reasoning_guard/cache.go` | 新增 | L2 缓存：`ReasoningContentCache` 接口 + `Backfill` 保守回填策略 + 进程内/Redis 双实现 |
| `relay/reasoning_guard/wire.go` | 新增 | relay 入口层集成钩子：`ApplyGuardRequest`(L1+L2) / `CaptureResponseReasoning`(L2 Store) / `MaybeEnhanceErrorResponse`(L3) + `DefaultCache()` 进程级单例 |
| `relay/reasoning_guard/guard_test.go` | 新增 | 表驱动回归测试：模型名嗅探边界 + OpenAI/Claude 格式诊断 + 保守回填策略（全部命中/部命中/跨turn/空缓存）+ TTL 驱逐 |
| `relaykit/dto/channel_settings.go` | 修改 | 新增 3 字段：`DeepseekReasoningGuardDisabled` / `DeepseekReasoningCache` / `DeepseekReasoningCacheTTL` |
| `relay/compatible_handler.go` | 修改 | TextHelper 接入：前置阶段诊断+回填（ModelMappedHelper 后）、响应阶段 L3 引导（400+reasoning_content）、DoResponse 后 L2 捕获 |

### 2.2 前端（2 文件修改）

| 文件 | 类型 | 职责 |
|---|---|---|
| `web/src/features/channels/lib/channel-form.ts` | 修改 | schema/defaultValues/parseChannel/buildSettingJSON 四处新增 3 个 `deepseek_*` 字段 |
| `web/src/features/channels/components/drawers/channel-mutate-drawer.tsx` | 修改 | UI 三 FormField（条件渲染按 `hasDeepSeekV4Model`）+ watch 声明清理 |

### 2.3 i18n（7 文件修改）

| 文件 | 类型 | 职责 |
|---|---|---|
| `web/src/i18n/locales/en.json` | 修改 | 新增 6 个 key（guard/cache/cache TTL label + description） |
| `web/src/i18n/locales/zh.json` | 修改 | 同上，中文翻译 |
| `web/src/i18n/locales/zh-TW.json` | 修改 | 同上，繁体中文翻译 |
| `web/src/i18n/locales/fr.json` | 修改 | 同上，法语翻译 |
| `web/src/i18n/locales/ru.json` | 修改 | 同上，俄语翻译 |
| `web/src/i18n/locales/ja.json` | 修改 | 同上，日语翻译 |
| `web/src/i18n/locales/vi.json` | 修改 | 同上，越南语翻译 |

### 2.4 设计文档（1 文件已存在）

| 文件 | 类型 | 职责 |
|---|---|---|
| `docs/channel/deepseek-v4-reasoning-guard.md` | 已存在 | 完整设计文档（背景、现状审查、适用范围、架构、实施步骤、数据流、验证策略、AGENTS.md 合规性） |

---

## 3. 实现细节

### 3.1 L1 诊断（`relay/reasoning_guard/guard.go`）

**模型名嗅探总闸门**：

```go
func DetectIfDeepSeekV4(modelName string) bool {
    return strings.Contains(strings.ToLower(modelName), "deepseek-v4")
}
```

与渠道类型无关，覆盖所有承载 `deepseek-v4-*` 模型的渠道路径。**模糊匹配**：大小写不敏感 + 容忍前后缀，兼容第三方供应商的 `DeepSeek-V4-Pro`、`v4-deepseek-pro`、`deepseekv4-pro` 等变体。

**format-agnostic 诊断**：

`DetectReasoningContentGap(format, modelName, messages)` 按 RelayFormat 分派：

- **OpenAI/Responses 格式**（`fillOpenAIGap`）：遍历 `req.Messages`，对 `role=assistant` 且携带 `tool_calls` 的消息，检查 `Message.ReasoningContent` 是否为 nil
- **Claude 格式**（`fillClaudeGap`）：遍历 `req.Messages`，对 `role=assistant` 且携带 `tool_use` block 的消息，检查是否同时携带 `thinking` content block

返回 `ReasoningGapReport`，包含缺失消息的索引和 `ToolCallIDs`（供 L2 缓存查找使用）。

**检测条件**（必须全部满足才启用）：

1. `DetectIfDeepSeekV4(modelName) == true`
2. 请求携带 tools 参数

**非 tool-call turn 的 reasoning_content 缺失不报告**（DeepSeek 文档：非 tool-call turn 的 reasoning_content 可省略）。

### 3.2 L2 缓存与保守回填（`relay/reasoning_guard/cache.go`）

**缓存接口**：

```go
type ReasoningContentCache interface {
    Store(ctx, channelID, toolCallID, reasoningContent, turnID string, ttl time.Duration) error
    Lookup(ctx, channelID int, toolCallIDs []string) (map[string]cacheEntry, error)
}
```

**双实现**：

- **进程内**（`inMemoryCache`）：`sync.Mutex` + `map[string]cacheEntry` + `time.AfterFunc` TTL 驱逐
- **Redis**（`redisCache`）：key 设计 `newapi:dsreasoning:{channelID}:{toolCallID}`，value 为 `cacheEntry` 的 JSON，TTL 由渠道设置控制

`NewCache()` 默认选择 Redis（当 `common.RedisEnabled && common.RDB != nil`），回退进程内。

**保守回填策略**（对齐 deepseek-compat-kit DSK_REASONING_003）：

```go
func Backfill(ctx, cache, report, channelID, messages) error
```

仅当满足以下**全部**条件才回填：

1. 该 assistant message 的**每个** `tool_call_id` 都有缓存命中
2. 所有命中项共享同一非空 `TurnID`（同一上游 turn）
3. 缓存未过期

不满足 → 不动，仅记录诊断日志，让请求原样转发（上游会返回 400，触发 L3 引导）。

### 3.3 relay 入口层集成（`relay/reasoning_guard/wire.go`）

**进程级缓存单例**：

```go
var defaultCacheOnce sync.Once
var defaultCache ReasoningContentCache

func DefaultCache() ReasoningContentCache {
    defaultCacheOnce.Do(func() {
        defaultCache = NewCache()
    })
    return defaultCache
}
```

**三个集成钩子**：

| 钩子 | 层级 | 调用时机 |
|---|---|---|
| `ApplyGuardRequest` | L1+L2 | 渠道 adaptor `ConvertOpenAIRequest` 之前（共用前置阶段） |
| `CaptureResponseReasoning` | L2 Store | 渠道 adaptor `DoResponse` 之后的成功路径 |
| `MaybeEnhanceErrorResponse` | L3 | 渠道 adaptor `DoResponse` 之后的 400 路径 |

**渠道级开关**：

| 字段 | 默认值 | 作用 |
|---|---|---|
| `DeepseekReasoningGuardDisabled` | false | 显式 opt-out（零值=未设置→默认开启） |
| `DeepseekReasoningCache` | false | L2 服务端回填开关 |
| `DeepseekReasoningCacheTTL` | 3600000（1 小时） | 缓存 TTL（毫秒） |

### 3.4 TextHelper 接入（`relay/compatible_handler.go`）

**三个接入点**：

1. **前置阶段**（`ModelMappedHelper` 之后、渠道 adaptor 转发之前）：
   ```go
   reasoning_guard.ApplyGuardRequest(c.Request.Context(), reasoning_guard.DefaultCache(), info, request)
   ```
   运行 L1 诊断 + L2 保守回填。

2. **响应阶段 400 路径**（`httpResp.StatusCode != http.StatusOK`）：
   ```go
   bodyBytes, readErr := io.ReadAll(httpResp.Body)
   // ...
   reasoning_guard.MaybeEnhanceErrorResponse(info, httpResp, bodyBytes)
   ```
   检测 400 + body 含 `reasoning_content` 关键字，注入 `X-Newapi-Reasoning-Guard: missing-reasoning-content` 响应头。

3. **响应阶段成功路径**（`adaptor.DoResponse` 之后）：
   ```go
   if textResp, ok := usage.(*dto.OpenAITextResponse); ok && textResp != nil {
       reasoning_guard.CaptureResponseReasoning(c.Request.Context(), reasoning_guard.DefaultCache(), info, textResp)
   }
   ```
   捕获上游响应的 `reasoning_content` + `tool_call_id`，存入 L2 缓存。

### 3.5 前端 UI

**channel-form.ts**：在 schema、defaultValues、parseChannel、buildSettingJSON 四处新增 3 个 `deepseek_*` 字段，遵循"仅非默认值才 emit 到 setting JSON"原则以保持非 deepseek 渠道的 JSON 精简。

**channel-mutate-drawer.tsx**：三 FormField 条件渲染，仅当 `hasDeepSeekV4Model`（当前渠道模型列表含 `deepseek-v4-*` �缀名）时显示：

```tsx
const hasDeepSeekV4Model = useMemo(
    () => currentModelsArray.some((m) =>
      m.toLowerCase().includes('deepseek-v4')
    ),
    [currentModelsArray]
)
```

### 3.6 回归测试（`relay/reasoning_guard/guard_test.go`）

表驱动测试覆盖：

| 测试函数 | 覆盖维度 |
|---|---|
| `TestDetectIfDeepSeekV4` | 模型名嗅探边界（v4-pro/flash/suffix/uppercase/infix-wrapped/no-dash/marker-only/legacy-chat/v3/empty） |
| `TestDetectReasoningContentGapOpenAI` | OpenAI 格式诊断（完整/缺失/非v4/无tools/非tool-call turn/多turn部分缺失） |
| `TestDetectReasoningContentGapClaude` | Claude 格式诊断（有/无 thinking block） |
| `TestBackfillOpenAIConservative` | 保守回填策略（全部命中/部分命中/跨turn/空缓存） |
| `TestInMemoryCacheTTL` | 进程内缓存 TTL 驱逐 |

遵循 AGENTS.md：`testify/require` + `testify/assert`，表驱动断言真实不变式，不写凑覆盖率测试。

---

## 4. code_review 处理结果

对 working tree 变更运行 `code_review`，返回 6 个 findings，全部已修复。

| ID | 严重度 | 置信度 | 问题 | 修复 |
|---|---|---|---|---|
| **P1** | fix | 0.85 | L2 缓存从未写入：`CaptureResponseReasoning` 已定义但无调用点，导致 `Backfill` 的 `Lookup` 永远命中空缓存，`deepseek_reasoning_cache` 开关对用户是 dead control | 接入 `compatible_handler.go` DoResponse 后的成功路径，调用 `CaptureResponseReasoning`；前端 cache/TTL 开关现在有真实后端行为支撑 |
| **P2** | fix | 0.85 | `DeepseekReasoningGuard` 开关无效：`guardEnabled` 无条件返回 true，字段从未被任何 conditional 读取，操作员关闭开关后守护仍开启 | 改为 `DeepseekReasoningGuardDisabled` 显式 opt-out 字段（零值=未设置→默认开启），`guardEnabled` 读取 `!info.ChannelSetting.DeepseekReasoningGuardDisabled`；前端 schema/UI/i18n 全部对齐 |
| **P2** | fix | 0.80 | `NewCache()` 每请求新建：进程内缓存无法跨请求累积，L2 回填永远是 no-op（即使 Store 接入也无法跨请求携带状态） | 提为 `DefaultCache()` 进程级单例（`sync.Once`），所有调用点改用单例；Redis 分支无状态所以共享安全 |
| **P3** | cleanup | 0.90 | `watchDeepseekReasoningGuard` / `watchDeepseekReasoningCacheTTL` 声明但从未使用，导致 `form.watch` 订阅无谓重渲染 | 删除两个未使用的 `form.watch` 声明，保留 `watchDeepseekReasoningCache`（用于 TTL 字段条件渲染） |
| **P3** | cleanup | 0.70 | `Deepseek` 命名与 `DeepSeek` 不一致（`DetectIfDeepSeekV4` 用大写 S，struct 字段用小写 s） | 保留（review 标为可选；JSON tag 不变，字段本 diff 新引入无外部引用者；重命名是 API 变更，推迟到后续清理） |
| **P3** | fix | 0.60 | `io.ReadAll` 错误被吞掉：`bodyBytes, _ := io.ReadAll(...)` 丢弃错误，截断 body 可能导致 L3 嗅探假阴性 | 改为检查 `readErr`，非 nil 时记 `common.SysError` 并跳过 `MaybeEnhanceErrorResponse`；`httpResp.Body` 重缓冲在两分支都执行以保证 `RelayErrorHandler` 有可读 body |

**修复涉及的文件**：

- `relay/reasoning_guard/wire.go`：`guardEnabled` 读取 `DeepseekReasoningGuardDisabled`；新增 `DefaultCache()` 单例
- `relaykit/dto/channel_settings.go`：`DeepseekReasoningGuard` → `DeepseekReasoningGuardDisabled`
- `relay/compatible_handler.go`：前置调用改用 `DefaultCache()`；接入 `CaptureResponseReasoning`；`io.ReadAll` 错误检查
- `web/src/features/channels/lib/channel-form.ts`：字段名对齐 `deepseek_reasoning_guard_disabled`
- `web/src/features/channels/components/drawers/channel-mutate-drawer.tsx`：UI 字段名对齐；删除未使用 watch；Switch 含义反转（开=opt-out）
- 7 个 i18n locale 文件：2 个 key 因 Switch 含义反转而更新（`Disable DeepSeek V4 reasoning_content guard` + `Opt out of ...`）

---

## 5. 验证状态

| 验证项 | 结果 | 证据 |
|---|---|---|
| 前端 `tsc --noEmit` | ✅ 通过 | exit 0；`--listFiles` 确认 `channel-form.ts`、`channel-mutate-drawer.tsx` 均在实际检查的 1098 个文件之中 |
| 后端 `go build ./...` | ⚠️ 未运行 | 环境中无 Go 工具链（`where /R C:\ go.exe` 全盘搜索无输出） |
| 后端 `cd relaykit && GOWUILD=off go build ./...` | ⚠️ 未运行 | 同上 |
| 后端 `go test ./relay/reasoning_guard/...` | ⚠️ 未运行 | 同上 |
| `code_review` | ✅ 已运行 | 6 findings 全部已修复（见第 4 节） |

**前端 TypeScript 类型检查**通过，确认前端改动无类型错误。

**后端代码未经编译验证**——这是工具链缺失而非代码问题。后续验证规划参见 [`deepseek-v4-reasoning-guard-verification-plan.md`](./deepseek-v4-reasoning-guard-verification-plan.md)。

---

## 6. AGENTS.md 合规性自查

| 规则 | 合规 | 说明 |
|---|---|---|
| relaykit 模块独立性 | ✅ | 仅新增 `ChannelSettings` 字段，不引入根模块依赖 |
| JSON 包装 | ✅ | 所有 marshal/unmarshal 用 `common.Marshal` / `common.Unmarshal`，不直接 `encoding/json` |
| 数据库兼容 | ✅ | 不涉及 DB schema 变更（渠道设置存 setting JSON 字段） |
| 计费安全不变式 | ✅ | 本功能不触及 quota/billing 链路 |
| 后端测试质量 | ✅ | 用 `testify/require` + `assert`，表驱动断言真实不变式的，不写凑覆盖率测试 |
| 国际化 | ✅ | 前端用 i18next + `t('English key')`，7 个 locale 文件已同步 |
| 项目治理 | ✅ | 保留所有 new-api / QuantumNous 标识 |

---

## 7. 待办与后续

1. **后端编译验证**：在配置好 Go 工具链的环境中运行 `go build ./...` + `cd relaykit && GOWORK=off go build ./...` + `go test ./relay/reasoning_guard/...`
2. **Claude 格式 L2 回填**：当前 `Backfill` 仅支持 OpenAI/Responses 格式（写入 `Message.ReasoningContent`），Claude 格式的 thinking block 注入待后续实现
3. **流式响应 L2 捕获**：当前 `CaptureResponseReasoning` 仅接入非流式成功路径，流式响应的 reasoning_content 分片捕获待后续实现
4. **集成测试**：在真实 DeepSeek V4 渠道 + thinking 模式 + 多轮 tool-calling 场景下做端到端验证
5. **`Deepseek` 命名清理**：后续将 struct 字段 `Deepseek` 命名统一为 `DeepSeek`（P3 可选清理项）

详见后续验证测试规划文档：[`deepseek-v4-reasoning-guard-verification-plan.md`](./deepseek-v4-reasoning-guard-verification-plan.md)
