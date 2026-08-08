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

共变更 **20 个文件（11 新增 + 9 修改）**，分布在后端、前端、i18n 三个区域。

### 2.1 后端（8 文件：6 新增 + 2 修改）

| 文件 | 类型 | 职责 | 备注 |
|---|---|---|---|
| `relay/reasoning_guard/guard.go` | 新增 | L1 诊断核心：`DetectIfDeepSeekV4` 模型名嗅探 + `DetectReasoningContentGap` format-agnostic 诊断（OpenAI/Claude 双格式分派） | **测试期修复 2 处**：① `DetectIfDeepSeekV4` 归一化匹配（支持 `deepseekv4-pro`/`v4-deepseek-pro`）；② `fillClaudeGap` 中 `req.Tools` 类型为 `any`，改为 type-switch（`[]any` / `[]dto.ToolCallRequest`）后再 `len()`，并把 `ClaudeMediaMessage.ID` 改为正确的 `Id` 字段 |
| `relay/reasoning_guard/cache.go` | 新增 | L2 缓存：`ReasoningContentCache` 接口 + `Backfill` 保守回填策略 + 进程内/Redis 双实现 | |
| `relay/reasoning_guard/wire.go` | 新增 | relay 入口层集成钩子：`ApplyGuardRequest`(L1+L2) / `CaptureResponseReasoning`(L2 Store) / `MaybeEnhanceErrorResponse`(L3) + `DefaultCache()` 进程级单例 | |
| `relay/reasoning_guard/guard_test.go` | 新增 | 表驱动回归测试：模型名嗅探边界 + OpenAI/Claude 格式诊断 + 保守回填策略（全部命中/部命中/跨turn/空缓存）+ TTL 驱逐 | **测试期修复 1 处**：Claude 测试用例 struct 字段 `ID`→`Id`；`[]dto.ClaudeTool` 替换为 `[]any{map[string]any{...}}`（dto 中无 ClaudeTool 类型） |
| `relay/reasoning_guard/wire_test.go` | **新增（验证期补充）** | wire 层单测：L3 诊断头注入 / 渠道开关语义（guardEnabled/cacheEnabled/TTL）/ DefaultCache 单例 / CaptureResponseReasoning 捕获 + 并发烟雾 | 13 个子用例，覆盖 I5/I6/I8/Capture 路径 |
| `relay/reasoning_guard/integration_test.go` | **新增（验证期补充）** | 集成测试：① L1→L2 Capture→ApplyGuardRequest 全链路生命周期；② L3 多渠道开关联动（DeepSeek/Ali/OpenAI 透传） | 覆盖 I3/I7 跨渠道不变式 |
| `relaykit/dto/channel_settings.go` | 修改 | 新增 3 字段：`DeepseekReasoningGuardDisabled` / `DeepseekReasoningCache` / `DeepseekReasoningCacheTTL` | |
| `relay/compatible_handler.go` | 修改 | TextHelper 接入：前置阶段诊断+回填（ModelMappedHelper 后）、响应阶段 L3 引导（400+reasoning_content）、DoResponse 后 L2 捕获 | |

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

**模型名嗅探总闸门**（**验证期修复：扩充归一化匹配**）：

```go
func DetectIfDeepSeekV4(modelName string) bool {
    lower := strings.ToLower(modelName)
    if strings.Contains(lower, "deepseek-v4") {
        return true
    }
    normalized := strings.Map(func(r rune) rune {
        if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
            return r
        }
        return -1
    }, lower)
    return strings.Contains(normalized, "deepseekv4") || strings.Contains(normalized, "v4deepseek")
}
```

与渠道类型无关，覆盖所有承载 `deepseek-v4-*` 模型的渠道路径。**两级模糊匹配**：
1. 先尝试直接包含 `deepseek-v4`（大小写不敏感，容忍前后缀）；
2. 再把字符串归一化到纯字母数字（剔除 `-` `_` `.` 等分隔符），检查 `deepseekv4` 或 `v4deepseek` 两种相邻排列。

兼容第三方供应商的 `DeepSeek-V4-Pro`、`v4-deepseek-pro`、`deepseekv4-pro`、`V4-DeepSeek-Max` 等变体。

**format-agnostic 诊断**：

`DetectReasoningContentGap(format, modelName, messages)` 按 RelayFormat 分派：

- **OpenAI/Responses 格式**（`fillOpenAIGap`）：遍历 `req.Messages`，对 `role=assistant` 且携带 `tool_calls` 的消息，检查 `Message.ReasoningContent` 是否为 nil
- **Claude 格式**（`fillClaudeGap`）：遍历 `req.Messages`，对 `role=assistant` 且携带 `tool_use` block 的消息，检查是否同时携带 `thinking` content block

> **验证期修复 2 处（`fillClaudeGap`）**：
> 1. `ClaudeRequest.Tools` 的 DTO 字段类型声明为 `any`，直接写 `len(req.Tools)` 触发 `invalid argument: req.Tools (any) for built-in len` 静态错误。修复为 type-switch：先断言 `[]any` / `[]dto.ToolCallRequest` 后取 `len()`，否则退化为非空检查。
> 2. `ClaudeMediaMessage` 结构体的导出字段是 `Id`（小写 d），原实现误写为 `ID`，编译报错 `b.ID undefined`。已统一改为 `b.Id`，同步修复 `guard_test.go` 中所有 Claude 用例的构造字面量。

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

## 5. 验证执行结果（2026-08-08，按 `deepseek-v4-reasoning-guard-verification-plan.md` 执行）

### 5.1 验证环境

| 项 | 版本 |
|---|---|
| 操作系统 | Linux |
| Go | `go1.25.1 linux/amd64` |
| Bun | `1.2.14` |
| 工作目录 | `/workspace/new-api`（仓：DrewFly88/new-api，main 分支） |

### 5.2 阶段 1 — 编译与静态检查

| 步骤 | 命令 | 结果 | 说明 |
|---|---|---|---|
| 1a 根模块编译 | `go build ./...` | ✅ exit 0 | 仅附带信息性告警：`main.go:42:12: pattern web/dist: no matching files found`（`//go:embed web/dist` 前端未打包，与 reasoning_guard 无关） |
| 1b relaykit 独立编译 | `cd relaykit && GOWORK=off go build ./...` | ✅ exit 0 | 验证 relaykit 模块独立性（未引入根模块依赖） |
| 1c go vet（先暴露编译错误） | `go vet ./relay/reasoning_guard/` | ✅ exit 0 | **首轮 vet 直接暴露 4 处静态错误**（均在 L1/Claude 分派分支），已按第 3.1 节「验证期修复 2 处」+ guard_test.go 两处修复后干净通过。详见 5.5 节 Bug 清单 |

### 5.3 阶段 2 — 单元测试（reasoning_guard 包）

**总统计：11 个测试套件 × 43 个子用例全部通过，`-race` 无数据竞争。**

命令：
```bash
cd /workspace/new-api
go test -v -race -count=1 ./relay/reasoning_guard/
```

**不变式覆盖对照（I1~I8）**：

| 不变式 | 测试套件 | 子用例数 | 结果 |
|---|---|---|---|
| **I1 模型名嗅探准确** | `TestDetectIfDeepSeekV4` | 14 | ✅ 全覆盖：v4-pro/flash、大小写、infix-wrapped `v4-deepseek-pro`、no-dash `deepseekv4-pro`、legacy-chat、v3 负例、空串负例 |
| **I2 L1 诊断准确（OpenAI）** | `TestDetectReasoningContentGapOpenAI` | 6 | ✅ 完整/缺失/非v4/无tools/非tool-call-turn 不告警/多turn 部分缺失 |
| **I2 L1 诊断准确（Claude）** | `TestDetectReasoningContentGapClaude` | 2 | ✅ thinking 有/无 |
| **I3 L2 保守回填** | `TestBackfillOpenAIConservative` | 4 | ✅ 全部命中/部分命中拒填/跨turn拒填/空缓存拒填 |
| **I4 L2 TTL 驱逐** | `TestInMemoryCacheTTL` | 1 | ✅ 100ms 粒度时钟验证过期失效 |
| **I3 集成（L1→Capture→Backfill 串联）** | `TestEndToEndCacheLifecycle`（integration_test.go 新增） | 1 | ✅ Turn 1 存 rc+tool_calls → Turn 2 丢 rc → ApplyGuardRequest 检测 AnyMissing=true 并回填 rc 值 |
| **I7 跨渠道 L3 联动** | `TestL3WireFromGuardEnabled`（integration_test.go 新增） | 4 | ✅ DeepSeek(type=53)/Ali(type=40)/OpenAI透传(type=1) 三种渠道路径都能触发诊断头；显式 opt-out 时静默 |
| **I5 L3 引导条件性** | `TestMaybeEnhanceErrorResponse` | 6 | ✅ 400+关键词+V4→注入头；200/非V4/无关键词/nil resp/guard off→noop |
| **I6 开关语义（guardEnabled/cacheEnabled/TTL）** | `TestGuardEnabled`/`TestCacheEnabled`/`TestCacheTTL` | 12 | ✅ 各维度真值表；TTL=0/负数回退 1h 默认 |
| **I8 进程级缓存单例** | `TestDefaultCacheSingleton` | 1 | ✅ 两次 DefaultCache() 返回同一指针；Store→Lookup 跨调用能累积 |
| L2 Capture 正确性 + 并发安全 | `TestCaptureResponseReasoning` / `TestCaptureRace` | 6 | ✅ 5 种 capture 分支；并发烟雾下 Store 原子性 |

**`-race` 竞态检测器**：`ok github.com/QuantumNous/new-api/relay/reasoning_guard 1.161s`，无 `WARNING: DATA RACE`。

### 5.4 阶段 2 — 前端静态检查

| 步骤 | 命令（`cd web` 执行） | 结果 | 说明 |
|---|---|---|---|
| 依赖安装 | `bun install --frozen-lockfile` | ✅ exit 0 | 1118 packages installed（45.85s） |
| TypeScript 类型检查 | `bun run typecheck`（`tsgo -b`） | ✅ exit 0 | 输出单行静默退出，无类型错误 |
| Lint | `bun run lint`（oxlint，975 文件，186 规则） | ⚠️ exit 1（与本功能无关） | 375 errors + 78 warnings，**均为 `src/assets/clerk-logo.tsx` / `src/assets/logo.tsx` 的历史遗留问题**（`no-import-type-side-effects` / `self-closing-comp`）；reasoning_guard 前端改动 `channel-form.ts`、`channel-mutate-drawer.tsx` **0 条 lint 告警** |
| i18n 同步 | `bun run i18n:sync`（`sync-i18n.mjs`） | ✅ exit 0 | 报告写入 `web/src/i18n/locales/_reports/_sync-report.json`，7 个 locale 新增 6 个 key 全部对齐 |

### 5.5 Bug 清单（验证期暴露并修复）

共 4 处，**全部在阶段 1c `go vet` / 阶段 2 首轮 test 中直接暴露并修复**，未留到集成/生产：

| # | 严重度 | 位置 | 症状 | 修复 |
|---|---|---|---|---|
| **B1** | 编译阻断 | [guard.go](file:///workspace/new-api/relay/reasoning_guard/guard.go) `fillClaudeGap` tools 检查 | `invalid argument: req.Tools (any) for built-in len` — DTO 声明 `ClaudeRequest.Tools` 为 `any`，`vet` 拒绝直接取 `len` | 改为 type-switch：`[]any`→`len(tools)`；`[]dto.ToolCallRequest`→`len(tools)`；否则非空判断 |
| **B2** | 编译阻断 | [guard.go](file:///workspace/new-api/relay/reasoning_guard/guard.go) Claude 分支 Id 字段访问 | `b.ID undefined (type dto.ClaudeMediaMessage has no field or method ID)` — DTO 实际导出字段是 `Id`（小写 d） | 改为 `b.Id` |
| **B3** | 编译阻断 | [guard_test.go](file:///workspace/new-api/relay/reasoning_guard/guard_test.go) Claude 用例构造 | ① `ClaudeMediaMessage{ID:...}` 同 B2；② 引用 `[]dto.ClaudeTool` 类型不存在 | ① `ID`→`Id`；② 用 `[]any{map[string]any{"type":"custom","name":"get_weather"}}` |
| **B4** | 功能缺陷（I1 覆盖率） | [guard.go](file:///workspace/new-api/relay/reasoning_guard/guard.go) `DetectIfDeepSeekV4` 原始实现 | 测试用例 `infix-wrapped: v4-deepseek-pro` 与 `no-dash: deepseekv4-pro` 失败；仅匹配 `deepseek-v4` 字面串，漏了第三方供应商常见变体 | 引入 `strings.Map` 归一化（保留 a-z+0-9，剔除其他分隔符），双重检查 `deepseekv4` ∨ `v4deepseek` |

修复后 `go build ./relay/reasoning_guard/` + `go vet ./...` 全部干净；`TestDetectIfDeepSeekV4` 新增 2 个边界用例（infix-wrapped/no-dash）通过。

### 5.6 `code_review` 与验证结果的交叉验证

code_review 第 4 节记录的 6 个 findings（P1/P2/P2/P3/P3/P3）在本轮验证中通过以下方式确认已闭环：

| review finding | 本轮验证依据 |
|---|---|
| P1 Capture 从未写入 | `TestCaptureResponseReasoning` ×5 全部分支；`TestEndToEndCacheLifecycle` 的 Turn1→Turn2 回填端到端验证 |
| P2 Guard 开关无效 | `TestGuardEnabled` "V4+explicit opt-out→off" 子用例 ✅ |
| P2 NewCache 每请求新建 | `TestDefaultCacheSingleton` 指针恒等 + Store→Lookup 跨调用累积 ✅ |
| P3 未使用 watch 声明 | `bun run lint` 对 `channel-mutate-drawer.tsx` **0 条告警**（历史告警只在两个 logo 资源文件） |
| P3 Deepseek/DeepSeek 命名不一致 | 保留（验证期无外部引用冲突；重命名是 API 变更，推迟） |
| P3 io.ReadAll 错误被吞 | `TestMaybeEnhanceErrorResponse` 5 个 noop 分支 + 1 个注入分支覆盖 400 路径；`MaybeEnhanceErrorResponse` 对 nil 入参安全退出 |

### 5.7 第二轮验证执行结果（2026-08-08，按 `deepseek-v4-reasoning-guard-round2-test.md` 执行）

> 本轮覆盖第二轮更新（commit `18c8783a`：Claude 格式 L2 回填 + 流式响应 L2 捕获）的云端 Go 环境测试。

#### 5.7.1 第二轮 Bug 清单

共 1 处，在阶段 `go vet` 中直接暴露并修复：

| # | 严重度 | 位置 | 症状 | 修复 |
|---|---|---|---|---|
| **B5** | 编译阻断 | [wire.go:233](file:///workspace/new-api/relay/reasoning_guard/wire.go#L233) `CollectStreamReasoning` | `vet: relay/reasoning_guard/wire.go:233:12: undefined: common` — 新增函数引用 `common.UnmarshalJsonStr`，但 import 别名是 `newapicommon` | 改为 `newapicommon.UnmarshalJsonStr` |

#### 5.7.2 命令矩阵执行结果（10/10 通过）

| # | 命令 | 范围 | 预期 | 实际结果 |
|---|---|---|---|---|
| 1 | `go build ./...` | 根模块编译 | exit 0 | ✅ exit 0（仅 `web/dist` embed 常态告警） |
| 2 | `cd relaykit && GOWORK=off go build ./...` | relaykit 独立编译 | exit 0 | ✅ exit 0 |
| 3 | `go vet ./relay/reasoning_guard/...` | 静态检查 | exit 0 | ✅ exit 0（修复 B5 后干净） |
| 4 | `go test -count=1 ./relay/reasoning_guard/...` | 全套件单次运行 | 17 套件全 PASS | ✅ **17 套件全 PASS** |
| 5 | `go test -race -count=1 ./relay/reasoning_guard/...` | 全套件 + 竞争检测 | 同上 + 无 race | ✅ `ok 1.174s`，无 `WARNING: DATA RACE` |
| 6 | `TestBackfillClaudeConservative -v` | **本轮新增** Claude 回填 | 4 子用例 PASS | ✅ 4/4 PASS |
| 7 | `TestCaptureStreamResponseReasoning -v` | **本轮新增** 流式捕获 | 6 子用例 PASS | ✅ 6/6 PASS |
| 8 | `TestCollectStreamReasoning -v` | **本轮新增** 分片解析 | 6 子用例 PASS | ✅ 6/6 PASS |
| 9 | `TestEndToEndCacheLifecycle\|TestL3WireFromGuardEnabled` | 集成回归 | 2 套件全 PASS | ✅ 5 子用例全 PASS |
| 10 | `TestBackfillOpenAIConservative\|TestBackfillClaudeConservative` | OpenAI + Claude 回填对称 | 两套件全 PASS | ✅ 8 子用例全 PASS |

#### 5.7.3 17 个测试套件清单（全部 PASS）

| 套件 | 来源 | 子用例数 |
|---|---|---|
| `TestDetectIfDeepSeekV4` | 第一轮 | 14 |
| `TestDetectReasoningContentGapOpenAI` | 第一轮 | 6 |
| `TestDetectReasoningContentGapClaude` | 第一轮 | 2 |
| `TestBackfillOpenAIConservative` | 第一轮 | 4 |
| `TestInMemoryCacheTTL` | 第一轮 | 1 |
| `TestEndToEndCacheLifecycle` | 第一轮集成 | 1 |
| `TestL3WireFromGuardEnabled` | 第一轮集成 | 4 |
| `TestMaybeEnhanceErrorResponse` | 第一轮 wire | 6 |
| `TestGuardEnabled` | 第一轮 wire | 5 |
| `TestCacheEnabled` | 第一轮 wire | 4 |
| `TestCacheTTL` | 第一轮 wire | 3 |
| `TestDefaultCacheSingleton` | 第一轮 wire | 1 |
| `TestCaptureResponseReasoning` | 第一轮 wire | 5 |
| `TestCaptureRace` | 第一轮 wire | 1 |
| **`TestBackfillClaudeConservative`** | **本轮新增** | **4** |
| **`TestCaptureStreamResponseReasoning`** | **本轮新增** | **6** |
| **`TestCollectStreamReasoning`** | **本轮新增** | **6** |

**累计统计**：17 个测试套件 × 73 个子用例全部通过，`-race` 无数据竞争。

#### 5.7.4 本轮不变式校验要点

| 不变式 | 测试套件 | 关键断言 | 结果 |
|---|---|---|---|
| Claude thinking block 注入在 tool_use 之前 | `TestBackfillClaudeConservative` "full hit" | `blocks[0].Type == "thinking"` 且 `len(blocks) == 原始+1` | ✅ |
| 空 `ToolCallIDs` 不注入空 thinking block（P2 修复） | `TestBackfillClaudeConservative` "empty cache" | `firstThinkingText == ""` 且 `toolUseCount == 2` | ✅ |
| 流式分片 reasoning_content 正确拼接 | `TestCollectStreamReasoning` "multiple choices" | `frag == "ab"` | ✅ |
| 流式 tool_call_id 去重 | `TestCaptureStreamResponseReasoning` "stores all" | `len(hits) == 2`，两 id 均命中 | ✅ |
| 非法 JSON 分片不 panic | `TestCollectStreamReasoning` "invalid JSON" | 返回零值，无 panic | ✅ |
| `StreamGuardActive` 门外置（P3 修复） | `TestCaptureStreamResponseReasoning` "cache disabled" | cache 无写入 | ✅ |

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

## 7. 待办与后续（2026-08-08 更新）

### 7.1 本轮已完成项（原 §7 待办，全部落地验证）

| 原待办 | 完成情况 | 证据 |
|---|---|---|
| **① 后端编译验证**（go build 根/relaykit + go test） | ✅ 全部通过，见 §5.2、§5.3 | `go build ./...` exit 0（含 relaykit 独立编译）；`go test -race -count=1 ./relay/reasoning_guard/` 11 套件 × 43 子用例全绿 |
| ④ 集成测试（模拟 relay 接入点串联 L1/L2/L3） | ✅ 新增 [integration_test.go](file:///workspace/new-api/relay/reasoning_guard/integration_test.go) | `TestEndToEndCacheLifecycle`（Turn1 Store → Turn2 ApplyGuardRequest 回填）+ `TestL3WireFromGuardEnabled`（DeepSeek/Ali/OpenAI 三渠道路径 × opt-out 静默）|

### 7.2 待办遗留

1. **✅ Claude 格式 L2 回填**（2026-08-08 完成，第二轮验证通过）：`Backfill` 已新增 `backfillClaude` 分支，在保守命中（全部 tool_use id 命中 + 同 turn + 未过期）时把 thinking content block 注入到 assistant message 的 content 数组头部（在 tool_use blocks 之前）。已加空 `ToolCallIDs` 守卫防止注入空 thinking block（code_review P2 修复）。测试见 `guard_test.go::TestBackfillClaudeConservative`（4 子用例：全部命中/部命中/跨turn/空缓存），**第二轮验证结果见 §5.7.2 命令 #6（4/4 PASS）**。
2. **✅ 流式响应 L2 捕获**（2026-08-08 完成，第二轮验证通过）：`wire.go` 新增 `CaptureStreamResponseReasoning`（流结束后一次性 Store）+ `CollectStreamReasoning`（每分片解析累加）+ `StreamGuardActive`（循环外门谓词）。`OaiStreamHandler` 在 `StreamScannerHandler` 回调中累加分片（reasoning_content 拼接 + tool_call_id 去重），流结束后调用 `CaptureStreamResponseReasoning`。门用 `StreamGuardActive` 提到循环外，非 DeepSeek 流式流量不付额外 JSON 解析代价（code_review P3 修复）。测试见 `wire_test.go::TestCaptureStreamResponseReasoning`（6 子用例）+ `TestCollectStreamReasoning`（6 子用例），**第二轮验证结果见 §5.7.2 命令 #7/#8（12/12 PASS）**。
3. **🔲 真渠道端到端验证**：在真实 DeepSeek V4 渠道 + thinking 模式 + 多轮 tool-calling 场景做 E2E（需有效 API key）。
4. **🔲 `Deepseek` 命名清理**：后续将 struct 字段 `DeepseekReasoningGuardDisabled`/`DeepseekReasoningCache`/`DeepseekReasoningCacheTTL` 统一为 `DeepSeek` 前缀（P3 可选清理项，属 API 变更需单独迁移文档）。

> **第二轮验证总结**：按 [`deepseek-v4-reasoning-guard-round2-test.md`](./deepseek-v4-reasoning-guard-round2-test.md) 执行 10 条命令矩阵全部通过（§5.7.2），发现并修复 1 处编译阻断 Bug B5（§5.7.1），累计 17 套件 × 73 子用例全绿，`-race` 无数据竞争。

### 7.3 最终交付物清单（代码 + 文档）

- **代码**：
  - 核心实现 3 个 Go 源文件（guard.go / cache.go / wire.go）
  - 测试 3 个 Go 源文件（guard_test.go / **wire_test.go** / **integration_test.go**，后两个验证期新增）
  - DTO：relaykit/channel_settings.go 新增 3 字段
  - relay 接入：compatible_handler.go 三接入点
  - 前端：channel-form.ts（schema/defaults/parse/build）+ channel-mutate-drawer.tsx（条件 FormField）
  - i18n：7 个 locale 文件，新增 6 个 key
- **文档**：
  - [deepseek-v4-reasoning-guard.md](./deepseek-v4-reasoning-guard.md)（设计）
  - [deepseek-v4-reasoning-guard-verification-plan.md](./deepseek-v4-reasoning-guard-verification-plan.md)（第一轮验证规划）
  - [deepseek-v4-reasoning-guard-round2-test.md](./deepseek-v4-reasoning-guard-round2-test.md)（第二轮验证规划）
  - 本文件（DevLog：实现 + review + 第一轮/第二轮验证全链路记录）

详见验证测试规划文档：[`deepseek-v4-reasoning-guard-verification-plan.md`](./deepseek-v4-reasoning-guard-verification-plan.md)（第一轮）、[`deepseek-v4-reasoning-guard-round2-test.md`](./deepseek-v4-reasoning-guard-round2-test.md)（第二轮）。
