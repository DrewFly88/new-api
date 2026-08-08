# DeepSeek V4 reasoning_content 守护 — 第二轮测试文档

> 本文档覆盖 **第二轮更新**（commit `18c8783a`：Claude 格式 L2 回填 + 流式响应 L2 捕获）的云端 Go 环境测试方案。
> 第一轮验证规划见 [`deepseek-v4-reasoning-guard-verification-plan.md`](deepseek-v4-reasoning-guard-verification-plan.md)。

## 1. 本轮变更摘要

| 变更 | 文件 | 说明 |
|---|---|---|
| Claude 格式 L2 回填 | `relay/reasoning_guard/cache.go` | 新增 `backfillClaude`：保守命中时把 thinking content block 注入到 assistant message 的 content 数组头部（在 tool_use blocks 之前） |
| 空 `ToolCallIDs` 守卫 | `relay/reasoning_guard/cache.go` | `backfillOpenAI` + `backfillClaude` 均加 `if len(miss.ToolCallIDs) == 0 { continue }`，防止注入空 thinking block（code_review P2 修复） |
| 流式 L2 捕获 | `relay/reasoning_guard/wire.go` | 新增 `CaptureStreamResponseReasoning`（流结束后一次性 Store）+ `CollectStreamReasoning`（每分片解析累加）+ `StreamGuardActive`（循环外门谓词） |
| OaiStreamHandler 接入 | `relay/channel/openai/relay-openai.go` | 在 `StreamScannerHandler` 回调中累加分片，门用 `StreamGuardActive` 提到循环外，流结束后调用 `CaptureStreamResponseReasoning`（code_review P3 修复） |
| 测试 | `guard_test.go` + `wire_test.go` | 新增 3 个测试套件共 16 个子用例 |

## 2. 本轮新增测试套件

| 套件 | 文件 | 子用例数 | 覆盖不变式 |
|---|---|---|---|
| `TestBackfillClaudeConservative` | `guard_test.go` | 4 | Claude 格式 L2 回填：全部命中注入 thinking block / 部命中不动 / 跨 turn 不动 / 空缓存不动 |
| `TestCaptureStreamResponseReasoning` | `wire_test.go` | 6 | 流式 L2 捕获：rc+ids 存储 / 空 rc 跳过 / 无 ids 跳过 / 空白 id 过滤 / 缓存禁用跳过 / nil cache 不 panic |
| `TestCollectStreamReasoning` | `wire_test.go` | 6 | 每分片解析：reasoning+toolid 提取 / 空分片零值 / 多 choice 累加 / 空 id 过滤 / 非法 JSON 不 panic / nil 不 panic |

## 3. 完整测试命令矩阵

> **无需新建任何测试脚本**——本轮测试已全部以 `*_test.go` 形式内置在 `relay/reasoning_guard/` 包中，`go test` 会自动发现。
> 命令中的 `/workspace/new-api` 替换为云端实际工作目录。

| # | 命令 | 范围 | 预期 | 判定 |
|---|---|---|---|---|
| 1 | `cd /workspace/new-api && go build ./...` | 根模块编译 | exit 0，无输出 | 退出码=0 |
| 2 | `cd /workspace/new-api/relaykit && GOWORK=off go build ./...` | relaykit 独立编译（AGENTS.md 强制） | exit 0，无输出 | 退出码=0 |
| 3 | `cd /workspace/new-api && go vet ./relay/reasoning_guard/...` | 静态检查 | exit 0，无输出 | 退出码=0 |
| 4 | `cd /workspace/new-api && go test -count=1 ./relay/reasoning_guard/...` | 全套件单次运行 | 17 套件全 PASS | 末行 `ok` 行无 FAIL |
| 5 | `cd /workspace/new-api && go test -race -count=1 ./relay/reasoning_guard/...` | 全套件 + 竞争检测 | 同上 + 无 race 报告 | 末行 `ok`，无 `WARNING: DATA RACE` |
| 6 | `cd /workspace/new-api && go test -run TestBackfillClaudeConservative -v -count=1 ./relay/reasoning_guard/` | **本轮新增** Claude 回填 | 4 子用例全 PASS | `--- PASS` 4 个 |
| 7 | `cd /workspace/new-api && go test -run TestCaptureStreamResponseReasoning -v -count=1 ./relay/reasoning_guard/` | **本轮新增** 流式捕获 | 6 子用例全 PASS | `--- PASS` 6 个 |
| 8 | `cd /workspace/new-api && go test -run TestCollectStreamReasoning -v -count=1 ./relay/reasoning_guard/` | **本轮新增** 分片解析 | 6 子用例全 PASS | `--- PASS` 6 个 |
| 9 | `cd /workspace/new-api && go test -run 'TestEndToEndCacheLifecycle\|TestL3WireFromGuardEnabled' -v -count=1 ./relay/reasoning_guard/` | 集成回归（L1→L2→回填全链路 + L3 多渠道） | 2 套件全 PASS | `--- PASS` 各子用例 |
| 10 | `cd /workspace/new-api && go test -run 'TestBackfillOpenAIConservative\|TestBackfillClaudeConservative' -v -count=1 ./relay/reasoning_guard/` | OpenAI + Claude 回填对称不变式 | 两套件全 PASS | `--- PASS` 合计 |

> **注**：命令 #9、#10 的正则 alternation 用 `\|`（POSIX BRE 兼容），Go test `-run` flag 使用 RE2 语法，实际写 `|` 即可。若云端 shell 对 `|` 有特殊解释，可改写为两次独立 `-run` 调用。

## 4. 本轮不变式校验要点

| 不变式 | 测试套件 | 关键断言 |
|---|---|---|
| Claude thinking block 注入在 tool_use 之前 | `TestBackfillClaudeConservative` "full hit" | `blocks[0].Type == "thinking"` 且 `len(blocks) == 原始+1` |
| 空 `ToolCallIDs` 不注入空 thinking block（P2 修复） | `TestBackfillClaudeConservative` "empty cache" | `firstThinkingText == ""` 且 `toolUseCount == 2` |
| 流式分片 reasoning_content 正确拼接 | `TestCollectStreamReasoning` "multiple choices" | `frag == "ab"` |
| 流式 tool_call_id 去重 | `TestCaptureStreamResponseReasoning` "stores all" | `len(hits) == 2`，两 id 均命中 |
| 非法 JSON 分片不 panic | `TestCollectStreamReasoning` "invalid JSON" | 返回零值，无 panic |
| `StreamGuardActive` 门外置（P3 修复） | `TestCaptureStreamResponseReasoning` "cache disabled" | cache 无写入 |

## 5. 推荐执行顺序

1. **编译先行**：命令 #1 → #2（根模块 + relaykit 独立编译，AGENTS.md 强制）
2. **静态检查**：命令 #3（`go vet`）
3. **本轮新增套件细粒度验证**：命令 #6 → #7 → #8（-v 输出每个子用例）
4. **全套件 + 竞争检测**：命令 #5（`-race`，确认 `TestCaptureRace` + `TestCaptureStreamResponseReasoning` 并发场景无 race）
5. **回归对称不变式**：命令 #9 → #10（确认本轮未破坏第一轮的 OpenAI 回填 / 集成链路）

## 6. 一次性通过判定

最简判定（编译 + 全套件含竞争检测一次跑完）：

```bash
cd /workspace/new-api && \
  go build ./... && \
  cd relaykit && GOWORK=off go build ./... && \
  cd .. && go test -race -count=1 ./relay/reasoning_guard/... 2>&1 | tail -5
```

**预期末行**形如：

```
ok      github.com/QuantumNous/new-api/relay/reasoning_guard    0.XXXs
```

**通过条件**：无 `FAIL` / `WARNING: DATA RACE` / `panic` / 编译错误。

## 7. 失败处置

| 失败现象 | 可能原因 | 下一步 |
|---|---|---|
| 命令 #1/#2 编译错误 | DTO 字段名/类型不匹配（如 `ClaudeMediaMessage.Id` vs `ID`） | 贴完整编译错误，可据报定位字段 |
| 命令 #5 race 报告 | `inMemoryCache` 锁遗漏或 `guardSeenToolIDs` 并发访问 | 贴 race 报告 stack，定位加锁点 |
| 命令 #6 "empty cache" 子例失败 | 空 `ToolCallIDs` 守卫未生效（P2 修复回归） | 确认 `cache.go` 中 `if len(miss.ToolCallIDs) == 0 { continue }` 存在 |
| 命令 #7 "cache disabled" 子例失败 | `StreamGuardActive` 门未生效（P3 修复回归） | 确认 `relay-openai.go` 中 `guardStreamActive` 在循环外计算且 `if guardStreamActive` 包裹 `CollectStreamReasoning` |
| 命令 #10 OpenAI 套件失败 | 本轮修改 `backfillOpenAI` 加守卫时引入回归 | 贴 `-v` 输出，对比第一轮验证结果 |

若任一命令失败，请把完整输出贴回，可据此定位修复。

## 8. 剩余待办（本轮不覆盖）

| 待办 | 状态 | 说明 |
|---|---|---|
| 真渠道端到端验证 | 🔲 | 需真实 DeepSeek V4 API key + thinking 模式 + 多轮 tool-calling |
| `Deepseek` → `DeepSeek` 命名清理 | 🔲 | P3 可选项，属 API 变更需单独迁移文档 |
