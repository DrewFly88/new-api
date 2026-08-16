# Rebase 同步验证文档

- 日期：2026-08-16
- 分支：`main`
- 目标：验证「本地 7 个开发提交 rebase 到上游 34 个提交之上」后的代码完整性，确认后方可推送至自有仓库。
- 验证环境：云端 Go + bun 环境（2026-08-16 实际执行全部构建与测试，非仅文件级检查）。

## 1. 背景与变更范围

本地 `main` 与上游 `https://github.com/QuantumNous/new-api` 曾在 `5c3abffe` 处分叉，本次执行：

```
git rebase origin/main
```

将本地 7 个开发提交重放到上游最新 `e2c7aa7b` 之上。rebase 全程无冲突，未产生任何冲突标记。

### 1.1 本地开发提交（rebase 后新 SHA，顺序从旧到新）

| 主题 | 提交 |
|---|---|
| docs: DeepSeek V4 reasoning_content guard design | `f36e0e7c` |
| feat: DeepSeek V4 reasoning_content 完整性防护 | `aaa283a5` |
| test: reasoning_guard wire/integration 测试与 L1 修复 | `281b1b0b` |
| feat: Claude L2 回填与流式 reasoning 捕获 | `c7df9038` |
| docs: round-2 测试计划 | `8ef97efd` |
| fix(wire.go): CollectStreamReasoning import alias 修正 | `60511732` |
| docs: Electron 打包指南 | `73e03f95` |

### 1.2 上游合并进来的内容（34 个提交，e2c7aa7b 及之前）

- 计费/充值：钱包充值原子结算、充值订单拒付守卫、异步任务退款同步 `used_quota`、条件倍率日志高亮等
- Relay：Claude 空 tools 注入修复、Responses 转换保留 presence/frequency penalty、Ollama reasoning 上下文、阿里图片模型映射修复等
- Web 前端：Vitest 测试标准化、移动端侧栏、Turnstile 刷新、token 轮换确认、搜索防抖等
- OAuth/认证、通道管理、依赖升级等

### 1.3 双方重叠文件（rebase 已自动合并）

- `web/src/features/channels/lib/channel-form.ts`
- `web/src/features/channels/components/drawers/channel-mutate-drawer.tsx`
- `web/src/i18n/locales/{en,zh,zh-TW,fr,ru,ja,vi}.json`（7 个）

## 2. 验证清单

### A. Git 状态验证

```bash
# 1) 确认位于 main 且工作区干净
git status
# 预期：On branch main / nothing to commit, working tree clean

# 2) 确认 7 个本地提交在远端之上、无落后
git log --oneline -12
# 预期：顶部为 73e03f95 (docs: add Electron packaging guide)，
#       依次往下为 60511732 → 8ef97efd → c7df9038 → 281b1b0b → aaa283a5 → f36e0e7c，
#       第 8 行起为上游 e2c7aa7b (test(web): standardize frontend tests on Vitest #6569)

git rev-list --count origin/main..HEAD   # 预期：7
git rev-list --count HEAD..origin/main   # 预期：0

# 3) 确认无冲突标记残留（应无任何输出）
grep -rn "^<<<<<<<\|^=======\|^>>>>>>>" --include="*.go" --include="*.ts" --include="*.tsx" --include="*.json" relay/ relaykit/ web/src/ common/ service/ model/ || echo "no conflict markers"
```

### B. Go 后端构建（云端执行）

```bash
go version   # 需要 Go 1.22+

# 根模块全量构建
GOWORK=off go build ./...
# 预期：退出码 0，无输出

# 根模块全量编译检查（含测试文件编译）
GOWORK=off go vet ./...
# 预期：退出码 0，无输出
```

### C. relaykit 独立模块（必须独立可构建）

```bash
cd relaykit && GOWORK=off go build ./...
# 预期：退出码 0，无输出

cd relaykit && GOWORK=off go test ./...
# 预期：全部 PASS（含 dto/channel_settings_test.go 等）
```

### D. Go 后端测试（云端执行）

```bash
# 本地开发功能相关测试（reasoning_guard 完整测试套件）
GOWORK=off go test ./relay/reasoning_guard/... -v
# 预期：guard_test.go / wire_test.go / integration_test.go 全部 PASS

# 上游计费相关回归测试（billingexpr / quota_math）
GOWORK=off go test ./pkg/billingexpr/... ./common/... 
# 预期：全部 PASS

# 全量测试（可选，耗时较长；等价于 make test 的根模块部分）
GOWORK=off go test ./...
```

### E. 前端验证（云端或具备 bun 的环境执行）

```bash
cd web
bun install --frozen-lockfile   # 预期：安装成功

# 类型检查（tsgo -b）
bun run typecheck
# 预期：退出码 0，无类型错误

# 前端测试（Vitest）
bun run test
# 预期：全部 PASS（上游新增 web/src/**/__tests__/* 测试）

# i18n 一致性检查：运行后应无 git diff（或仅新增缺失 key 的预期变化）
bun run i18n:sync
git status --short
# 预期：若之前 i18n 文件已同步，则无改动；若有改动，需人工确认 key 变更合理

# 生产构建（可选，较耗时）
bun run build
# 预期：构建成功，web/dist 产出
```

### F. 关键文件存在性核对

```bash
ls relay/reasoning_guard/   # 预期含：cache.go guard.go guard_test.go integration_test.go wire.go wire_test.go
ls relaykit/go.mod          # 预期存在（独立模块）
head -3 go.mod              # 预期：module github.com/QuantumNous/new-api
```

## 3. 预期结果汇总

| 检查项 | 预期结果 |
|---|---|
| git status | 干净，main 分支 |
| ahead / behind | 7 / 0 |
| 冲突标记 | 无 |
| 根模块 go build / go vet | 通过 |
| relaykit 独立 build / test | 通过 |
| reasoning_guard 测试 | 全部 PASS |
| billingexpr / common 测试 | 全部 PASS |
| 前端 typecheck / test / build | 通过 |
| i18n 一致性 | 无意外改动 |

## 4. 收尾

验证全部通过后：

```bash
# 推送到自有仓库（默认分支为 main）
git push drewfly main
```

> 注意：`origin`（上游 QuantumNous/new-api）**不推送**，仅保留 fetch 同步关系。

## 5. 验证执行结果（2026-08-16）

实际在云端沙箱（Go 1.22+、bun 1.2.14）执行全部检查项，均通过。

| 检查项 | 实际命令 | 结果 |
|---|---|---|
| A. Git 状态 | `git status` / `git rev-list --count origin/main..HEAD` | ✅ 干净，main 分支，ahead 7 / behind 0，无冲突标记 |
| B. 根模块构建 | `GOWORK=off go build ./...` | ✅ 退出码 0，无输出 |
| B. 根模块 vet | `GOWORK=off go vet ./...` | ✅ 退出码 0，无输出 |
| C. relaykit 独立 build | `cd relaykit && GOWORK=off go build ./...` | ✅ 退出码 0，无输出 |
| C. relaykit 独立 test | `cd relaykit && GOWORK=off go test ./...` | ✅ 全部 PASS |
| D. reasoning_guard 测试 | `GOWORK=off go test ./relay/reasoning_guard/... -v` | ✅ 全部 PASS（guard / wire / integration 完整套件，含竞态测试） |
| D. 计费回归测试 | `GOWORK=off go test ./pkg/billingexpr/... ./common/...` | ✅ 全部 PASS |
| E. 前端依赖 | `cd web && bun install --frozen-lockfile` | ✅ 1200 packages 安装成功 |
| E. typecheck | `bun run typecheck`（tsgo -b） | ✅ 退出码 0，无类型错误 |
| E. 前端测试 | `bun run test`（Vitest） | ✅ 31 文件 / 156 测试全部通过 |
| E. i18n 一致性 | `bun run i18n:sync` + `git status --short` | ✅ 完成后工作区干净，无意外改动 |
| F. 关键文件 | `ls relay/reasoning_guard/` / `ls relaykit/go.mod` / `head -3 go.mod` | ✅ 6 文件齐全，relaykit 独立模块存在，module 为 `github.com/QuantumNous/new-api` |

**结论**：所有检查项符合预期，本地 7 个开发提交已正确重放至上游 `e2c7aa7b` 之上，代码完整、可构建、可测试，具备推送条件。
