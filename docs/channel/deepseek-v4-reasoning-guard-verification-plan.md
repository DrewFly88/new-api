# 后续验证测试规划 — DeepSeek V4 reasoning_content 完整性守护

> 本文档规划在实施完成（见 [`deepseek-v4-reasoning-guard-devlog.md`](./deepseek-v4-reasoning-guard-devlog.md)）后，对 DeepSeek V4 reasoning_content 完整性守护功能进行的后续验证测试。
>
> **背景**：实施阶段因环境中无 Go 工具链，后端 `go build` / `go test` 均未运行；前端 `tsc --noEmit` 已通过。本规划覆盖未运行的编译/单元验证、新增的集成验证、以及端到端场景验证。
>
> 设计依据：[`deepseek-v4-reasoning-guard.md`](./deepseek-v4-reasoning-guard.md) 第 7 节"验证策略"

---

## 1. 验证目标

确认 DeepSeek V4 reasoning_content 守护功能满足以下不变式：

| 不变式 | 验证目的 |
|---|---|
| **I1 模型名嗅探准确性** | `DetectIfDeepSeekV4` 模糊匹配 `deepseek-v4`（大小写不敏感 + 容忍前后缀）对 V4 变体返回 true，对 legacy/v3/无关模型返回 false |
| **I2 L1 诊断准确性** | `DetectReasoningContentGap` 仅在 DeepSeek V4 + 携带 tools + assistant tool-call message 缺失 reasoning_content 时报告缺失；非 tool-call turn / 非 V4 模型 / 无 tools 不报告 |
| **I3 L2 保守回填安全性** | `Backfill` 仅在全部 tool_call_id 命中 + 同 turn + 未过期时回填；部分命中/跨turn/空缓存/查询错误均不回填 |
| **I4 L2 缓存 TTL 驱逐** | 进程内缓存在 TTL 过期后驱逐条目；Redis 缓存遵守过期 |
| **I5 L3 引导条件性** | `MaybeEnhanceErrorResponse` 仅在 400 + body 含 "reasoning_content" + DeepSeek V4 模型时设置诊断头 |
| **I6 渠道开关语义** | `DeepseekReasoningGuardDisabled=true` 关闭守护；`DeepseekReasoningCache=true` + `DeepseekReasoningGuardDisabled=false` 启用 L2 |
| **I7 跨渠道路径覆盖** | 守护逻辑对 DeepSeek 渠道、ali 渠道、OpenAI 透传、聚合渠道均生效（按模型名而非渠道类型） |
| **I8 进程级缓存单例** | `DefaultCache()` 在进程生命周期内返回同一实例；进程内缓存能跨请求累积 |
| **I9 前端 UI 条件渲染** | 三 FormField 仅在渠道模型列表模糊匹配 `deepseek-v4`（大小写不敏感 + 容忍前后缀）时显示；TTL 字段仅在 cache 开关打开时显示 |
| **I10 前端字段序列化** | `deepseek_reasoning_guard_disabled` / `deepseek_reasoning_cache` / `deepseek_reasoning_cache_ttl_ms` 正确序列化到 setting JSON；非默认值才 emit |

---

## 2. 验证阶段

### 阶段 1：编译与静态检查（环境前置）

**前置条件**：配置好 Go 工具链（Go 1.22+）和 Bun。

| 验证项 | 命令 | 通过标准 | 覆盖不变式 |
|---|---|---|---|
| 根模块编译 | `go build ./...` | 无错误 | 全部（编译前置） |
| relaykit 独立编译 | `cd relaykit && GOWORK=off go build ./...` | 无错误（AGENTS.md 强制要求） | 全部（模块独立性） |
| go vet | `go vet ./relay/reasoning_guard/... ./relay/compatible_handler.go ./relaykit/dto/channel_settings.go` | 无警告 | 全部（静态分析） |
| 前端 tsc | `cd web && bun run typecheck` | 无 TS 错误 | I9, I10 |
| 前端 lint | `cd web && bun run lint` | 无 oxlint 错误 | I9, I10 |
| 前端 i18n 同步检查 | `cd web && bun run i18n:sync`（dry-run 模式若有） | 7 个 locale 文件 key 对齐，无缺失 | I9（i18n 完整性） |

**预期可能的问题**：

- `wire.go` 的 `context` 导入在 `inMemoryCache.Store`/`Lookup` 中用 `_ context.Context` 接收但未使用——Go 允许参数未使用，但 `go vet` 可能提示
- `cache.go` 的 `redisCache` 若 `common.RedisGet`/`common.RedisSet` 签名与调用不符会编译失败——需对照 `common/redis.go` 实际签名
- `wire.go` 的 `currentModelName` 在 `info.ChannelMeta == nil` 时返回 `""`，与 `guardEnabled` 的 nil 检查冗余但无害

### 阶段 2：单元测试（纯函数 + 缓存）

运行已实现的 `relay/reasoning_guard/guard_test.go` + 新增补充测试。

| 验证项 | 命令 | 通过标准 | 覆盖不变式 |
|---|---|---|---|
| 现有测试 | `go test ./relay/reasoning_guard/... -v` | 全部通过 | I1, I2, I3, I4 |
| 竞态测试（进程内缓存） | `go test -race ./relay/reasoning_guard/...` | 无 data race | I4, I8（线程安全） |
| 基准测试（可选） | `go test -bench=. ./relay/reasoning_guard/...` | 诊断用，无硬性通过标准 | 性能基线 |

**需新增的补充单元测试**（阶段 1 编译通过后补充）：

#### 2.1 L3 引导单元测试

```
文件：relay/reasoning_guard/wire_test.go（新增）

TestMaybeEnhanceErrorResponse:
  - 400 + body 含 "reasoning_content" + DeepSeek V4 模型 → 设置诊断头，返回 true
  - 400 + body 含 "reasoning_content" + 非 DeepSeek V4 模型 → 不设置，返回 false
  - 200 + body 含 "reasoning_content" → 不设置（非 400）
  - 400 + body 不含 "reasoning_content" → 不设置
  - nil httpResp → 不设置
  - DeepseekReasoningGuardDisabled=true → 不设置（守护关闭）
```

#### 2.2 渠道开关单元测试

```
文件：relay/reasoning_guard/wire_test.go（新增）

TestGuardEnabled:
  - DeepSeek V4 模型 + DeepseekReasoningGuardDisabled=false → true
  - DeepSeek V4 模型 + DeepseekReasoningGuardDisabled=true → false（显式 opt-out）
  - 非 DeepSeek V4 模型 → false（无论开关）
  - nil info → false
  - nil ChannelMeta → false

TestCacheEnabled:
  - DeepseekReasoningCache=true + guard 启用 → true
  - DeepseekReasoningCache=false → false
  - DeepseekReasoningCache=true + guard 禁用 → false（守护关闭时缓存无意义）

TestCacheTTL:
  - DeepseekReasoningCacheTTL=7200000 → 返回 2 小时
  - DeepseekReasoningCacheTTL=0（未设置）→ 返回 1 小时（默认）
  - DeepseekReasoningCacheTTL=-1（非法）→ 返回 1 小时（兜底）
```

#### 2.3 进程级缓存单例测试

```
文件：relay/reasoning_guard/wire_test.go（新增）

TestDefaultCacheSingleton:
  - 多次调用 DefaultCache() 返回同一指针
  - Store 后 Lookup 能命中（跨调用累积）
```

#### 2.4 CaptureResponseReasoning 单元测试

```
文件：relay/reasoning_guard/wire_test.go（新增）

TestCaptureResponseReasoning:
  - cacheEnabled + resp 携带 reasoning_content + tool_calls → Store 被调用，后续 Lookup 命中
  - cacheEnabled + resp 无 reasoning_content → 不 Store
  - cacheEnabled + resp 无 tool_calls → 不 Store
  - cacheDisabled → 不 Store
  - nil resp → 不 Store
```

**测试编写约束**（遵循 AGENTS.md）：

- 用 `github.com/stretchr/testify/require` 做 setup 和 fatal 断言
- 用 `github.com/stretchr/testify/assert` 做非 fatal 值检查
- 表驱动，显式输入和精确期望输出
- 需要渠道状态时在 fixture 内显式初始化 `RelayInfo` + `ChannelSetting`
- 不写 fake fuzz/stress/performance 测试

### 阶段 3：集成测试（relay 链路）

验证守护逻辑在真实 relay 链路中的接入正确性。

| 验证项 | 方法 | 通过标准 | 覆盖不变式 |
|---|---|---|---|
| TextHelper 前置接入 | 在 `relay/compatible_handler_test.go`（新增）中构造 DeepSeek V4 + tools + 缺失 reasoning_content 的请求，调用 TextHelper，断言请求被回填（当缓存有命中）或原样转发（当缓存空） | 回填正确触发或保守跳过 | I2, I3, I7 |
| TextHelper L3 接入 | Mock 上游返回 400 + body 含 "reasoning_content"，断言响应头含 `X-Newapi-Reasoning-Guard` | 诊断头设置 | I5, I7 |
| TextHelper L2 捕获 | Mock 上游返回 200 + reasoning_content + tool_calls，断言缓存被写入 | 缓存条目存在 | I3, I8 |
| 跨渠道路径 | 对 ali 渠道（Claude 格式）构造相同场景，断言守护逻辑同样触发 | ali 路径覆盖 | I7 |

**集成测试 fixture 需求**：

- 构造 `RelayInfo`：设置 `UpstreamModelName="deepseek-v4-pro"`、`RelayFormat=RelayFormatOpenAI`、`ChannelSetting`（含 `DeepseekReasoningCache=true`）
- Mock `adaptor.DoRequest` / `adaptor.DoResponse`：分别返回构造的 400/200 响应
- 用 `httptest` 构造上游响应体

### 阶段 4：端到端场景验证（手动 + 可选自动化）

在真实 DeepSeek V4 API（或 mock 上游）下做多轮 tool-calling 全链路验证。

#### 4.1 场景矩阵

| 场景 | 入站 messages | 渠道设置 | 期望行为 | 覆盖不变式 |
|---|---|---|---|---|
| **S1 完整回传** | assistant tool-call message 携带 reasoning_content | guard=on, cache=off | 请求原样转发上游，上游返回 200 | I2（无缺失不报告） |
| **S2 缺失 + 无缓存** | assistant tool-call message 缺失 reasoning_content | guard=on, cache=off | 请求原样转发（不阻断），上游返回 400，响应头含 `X-Newapi-Reasoning-Guard` | I2, I5 |
| **S3 缺失 + 缓存命中同turn** | Turn 1 响应被捕获，Turn 2 缺失但缓存命中 | guard=on, cache=on | Turn 2 请求被回填 reasoning_content，上游返回 200 | I3, I8 |
| **S4 缺失 + 缓存部分命中** | Turn 2 的 tool_call_id 仅部命中缓存 | guard=on, cache=on | 不回填，原样转发，上游返回 400，触发 L3 | I3（保守策略） |
| **S5 缺失 + 缓存跨turn命中** | 缓存命中但来自不同 turn | guard=on, cache=on | 不回填，原样转发，上游返回 400，触发 L3 | I3（保守策略） |
| **S6 缓存过期** | 缓存命中但 TTL 已过 | guard=on, cache=on, TTL=1s | 不回填（缓存驱逐），原样转发，上游返回 400 | I4 |
| **S7 守护关闭** | 缺失 reasoning_content | guard=off (DeepseekReasoningGuardDisabled=true) | 不诊断、不回填、不引导；原样转发，上游 400 无诊断头 | I6 |
| **S8 非 DeepSeek V4 模型** | 缺失 reasoning_content | guard=on | 不诊断、不引导（模型名模糊匹配未命中） | I1, I7 |
| **S9 ali 渠道 Claude 格式** | Claude 格式 assistant 缺 thinking block | guard=on, cache=off | 诊断报告缺失，原样转发，上游 400，触发 L3 | I7（跨渠道） |
| **S10 OpenAI 透传渠道** | OpenAI 类型渠道配 deepseek-v4-pro 模型 | guard=on | 守护逻辑同样触发（按模型名而非渠道类型） | I7 |
| **S11 第三方变体模型名** | 渠道配 `DeepSeek-V4-Pro`/`v4-deepseek-pro`/`deepseekv4-pro` 等变体 | guard=on | 模糊匹配命中，守护逻辑同样触发 | I1（模糊匹配） |

#### 4.2 场景执行步骤

每个场景的标准化执行流程：

1. **环境准备**：
   - 启动 new-api 实例（配置 DeepSeek V4 渠道，指向真实 API 或 mock 上游）
   - 若用 mock 上游：用 `httptest.Server` 或 mock 工具返回构造的响应

2. **渠道配置**：
   - 在管理面板创建/编辑渠道，设置模型列表含 `deepseek-v4-pro`
   - 设置 `deepseek_reasoning_guard` / `deepseek_reasoning_cache` / `deepseek_reasoning_cache_ttl_ms`（按场景矩阵）

3. **请求构造**：
   - 用 curl 或 HTTP 客户端发送多轮 tool-calling 请求
   - Turn 1：用户消息 + tools，获取 assistant 的 tool_calls + reasoning_content
   - Turn 2：构造含 assistant tool-call message 但缺失/保留 reasoning_content 的请求（按场景）

4. **断言**：
   - 检查响应状态码（200/400）
   - 检查响应头 `X-Newapi-Reasoning-Guard`（存在/不存在）
   - 检查上游是否收到回填的 reasoning_content（通过 mock 上游日志或真实 API 行为）
   - 检查缓存状态（通过 Redis CLI 或进程内日志）

5. **记录**：
   - 每个场景的输入、期望、实际结果、通过/失败状态
   - 失败场景附上请求/响应原文和日志

#### 4.3 可选自动化

若团队有 E2E 测试基础设施（如 Playwright + mock 上游），可将场景矩阵自动化：

- 用 JSON fixture 描述每个场景的入站请求、渠道设置、期望响应
- 用 mock 上游（如 `httptest.Server`）返回构造的 DeepSeek V4 响应
- 自动化执行全部 10 个场景并生成报告

---

## 3. 验证执行顺序

```
阶段 1（编译与静态检查）
  │
  ├── 失败 → 修复编译错误，回到阶段 1
  │
  ▼ 通过
阶段 2（单元测试）
  │
  ├── 失败 → 修复逻辑错误，回到阶段 2
  │
  ▼ 通过
阶段 3（集成测试）
  │
  ├── 失败 → 修复接入问题，回到阶段 3
  │
  ▼ 通过
阶段 4（端到端场景验证）
  │
  ├── 失败 → 诊断端到端问题，修复后回到失败场景
  │
  ▼ 全部场景通过
验证完成 ✅
```

---

## 4. 工具链前置准备

### 4.1 Go 工具链

当前环境中无 Go 编译器。验证前需安装：

```bash
# 方式 1：winget（Windows）
winget install GoLang.Go

# 方式 2：官方安装包
# 从 https://go.dev/dl/ 下载 Windows 安装包并安装

# 验证
go version  # 期望 go1.22+
```

### 4.2 Bun（前端）

当前环境中无 Bun。验证前需安装：

```bash
# 方式 1：npm
npm install -g bun

# 方式 2：官方脚本
powershell -c "irm bun.sh/install.ps1 | iex"

# 验证
bun --version
```

### 4.3 Redis（L2 缓存测试，可选）

若要验证 Redis 实现：

```bash
# 本地启动 Redis
redis-server

# 或用 Docker
docker run -d -p 6379:6379 redis:7

# 验证
redis-cli ping  # 期望 PONG
```

### 4.4 DeepSeek API Key（端到端验证，可选）

若要验证真实 API 场景：

- 从 DeepSeek 平台获取 API Key
- 配置渠道的 API Key 字段
- 注意：真实 API 调用会产生计费

---

## 5. 测试文件清单

| 文件 | 阶段 | 状态 | 说明 |
|---|---|---|---|
| `relay/reasoning_guard/guard_test.go` | 阶段 2 | ✅ 已实现 | I1-I4 的表驱动测试 |
| `relay/reasoning_guard/wire_test.go` | 阶段 2 | ⚠️ 待新增 | I5-I6, I8, CaptureResponseReasoning 的单元测试 |
| `relay/compatible_handler_test.go` | 阶段 3 | ⚠️ 待新增 | I7 集成测试（TextHelper 接入验证） |
| E2E 场景 fixture | 阶段 4 | ⚠️ 待新增 | S1-S10 端到端场景（可选自动化） |

---

## 6. 验证通过标准

验证完成的条件：

| 条目 | 必须 | 说明 |
|---|---|---|
| 阶段 1 全部通过 | ✅ | 编译是后续验证的前置 |
| 阜段 2 现有测试通过 | ✅ | I1-I4 是核心不变式 |
| 阜段 2 补充测试通过 | ✅ | I5-I8 是 code_review 修复后的新增不变式 |
| 阜段 3 集成测试通过 | ✅ | I7 是设计文档的核心约束（跨渠道覆盖） |
| 阜段 4 场景 S1-S8 通过 | ✅ | 核心端到端场景 |
| 阜段 4 场景 S9-S10 通过 | 建议 | 跨渠道端到端（ali/OpenAI 透传） |
| 阜段 4 自动化 | 建议 | 若团队有 E2E 基础设施 |
| Redis 实现测试 | 建议 | 若部署环境用 Redis |
| 真实 API 端到端 | 建议 | 若有 API Key 和计费预算 |

---

## 7. 风险与回滚

### 7.1 验证中可能发现的问题

| 风险 | 可能性 | 影响 | 回滚方案 |
|---|---|---|---|
| `common.RedisGet`/`RedisSet` �名名或签名不符 | 中 | Redis 实现编译失败 | 对照 `common/redis.go` 实际签名修正 `cache.go` |
| `dto.OpenAITextResponse` 结构不匹配 | 低 | `CaptureResponseReasoning` 编译失败 | 对照 `relaykit/dto/openai_response.go` 实际字段修正 |
| `RelayInfo` 缺少 `GetChannelID` / `RequestId` 字段 | 中 | `wire.go` 编译失败 | 对照 `relay/common/relay_info.go` 实际方法修正 |
| `ClaudeMediaMessage` 结构不匹配 | 低 | `fillClaudeGap` 编译失败 | 对照 `relaykit/dto/claude.go` 实际字段修正 |
| L2 缓存回填破坏上游请求 | 低 | 客户端收到意外响应 | 关闭 `deepseek_reasoning_cache` 开关即回滚 |
| L3 引导头注入位置错误 | 低 | 响应头未到达客户端 | 检查 `httpResp.Header.Set` 在 `RelayErrorHandler` 之前调用 |

### 7.2 回滚机制

- **渠道级回滚**：设置 `DeepseekReasoningGuardDisabled=true` 即可完全关闭守护功能（对单渠道）
- **全局回滚**：在代码中移除 `compatible_handler.go` 的三处接入点即可恢复原行为（guard 包保留但不被调用）
- **紧急回滚**：git revert 守护功能的 commit（变更集中在 `relay/reasoning_guard/` + `compatible_handler.go` + `channel_settings.go`，回滚干净）

---

## 8. 时间估算

| 阶段 | 估算工时 | 说明 |
|---|---|---|
| 阜段 1：编译与静态检查 | 0.5-1 小时 | 含工具链安装 |
| 阜段 2：单元测试（含新增补充测试） | 3-4 小时 | 含编写 `wire_test.go` |
| 阜段 3：集成测试 | 4-6 小时 | 含编写 `compatible_handler_test.go` + fixture |
| 阜段 4：端到端场景验证 | 2-4 小时 | 手动执行 10 个场景；自动化另加 4-6 小时 |
| **合计** | **9.5-15 小时** | 不含可选自动化和真实 API 验证 |

---

## 9. 附录：场景请求示例

### 9.1 S2 场景（缺失 + 无缓存）请求示例

**Turn 1 请求**：

```bash
curl -X POST http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [
      {"role": "user", "content": "What'\''s the weather in Paris?"}
    ],
    "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}],
    "thinking": {"type": "enabled"}
  }'
```

**Turn 1 响应**（获取 tool_calls + reasoning_content）：

```json
{
  "choices": [{
    "message": {
      "role": "assistant",
      "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}],
      "reasoning_content": "I need to call get_weather for Paris."
    }
  }]
}
```

**Turn 2 请求**（缺失 reasoning_content，模拟客户端框架丢失）：

```bash
curl -X POST http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [
      {"role": "user", "content": "What'\''s the weather in Paris?"},
      {"role": "assistant", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]},
      {"role": "tool", "tool_call_id": "call_1", "content": "sunny, 20C"},
      {"role": "user", "content": "And tomorrow?"}
    ],
    "tools": [{"type": "function", "function": {"name": "get_weather"}}],
    "thinking": {"type": "enabled"}
  }'
```

**期望**：上游返回 400，响应头含 `X-Newapi-Reasoning-Guard: missing-reasoning-content`。

### 9.2 S3 场景（缺失 + 缓存命中同turn）请求示例

在 S2 基础上，渠道设置 `deepseek_reasoning_cache=true`，先执行一次完整 Turn 1（缓存被捕获），再执行 Turn 2（缺失但缓存命中）。

**期望**：Turn 2 请求被回填 reasoning_content，上游返回 200。

---

## 10. 参考

- 设计文档：[`deepseek-v4-reasoning-guard.md`](./deepseek-v4-reasoning-guard.md)
- DevLog：[`deepseek-v4-reasoning-guard-devlog.md`](./deepseek-v4-reasoning-guard-devlog.md)
- AGENTS.md：项目约定（测试质量、JSON 包装、relaykit 独立性等）
- deepseek-compat-kit：<https://github.com/xiaoshuo1988130/deepseek-compat-kit>
- DeepSeek 官方文档：<https://api-docs.deepseek.com/guides/thinking_mode/>
