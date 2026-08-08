# new-api Electron 桌面应用打包指南

> 本文档基于仓库现状（`electron/` 目录 + `.github/workflows/electron-build.yml`）编写，
> 描述如何将 new-api（Go 后端 + React 前端）打包为 Electron 桌面应用。

## 1. 现状总览

new-api 项目**已内置完整的 Electron 打包骨架**，无需从零搭建：

| 组件 | 文件 | 作用 |
|---|---|---|
| 主进程 | `electron/main.js` | spawn `new-api` 后端二进制（固定端口 3000）、创建 BrowserWindow、系统托盘、崩溃日志分析与错误提示 |
| 预加载 | `electron/preload.js` | 渲染进程安全桥接（contextBridge） |
| 打包配置 | `electron/package.json` | electron 39.8.5 + electron-builder 26.7.0；三平台 targets 与 extraResources |
| 本地脚本 | `electron/build.sh` | 一键构建：web build → go build → electron-builder（按 OS 分支） |
| CI workflow | `.github/workflows/electron-build.yml` | tag 触发：bun build → go build → npm install → `build:win` → 上传 GitHub Release |
| 图标/权限 | `icon.png`、`tray-icon*.png`、`entitlements.mac.plist` | 三平台图标 + macOS entitlements |

**运行架构**：

```
Electron 主进程 (main.js)
  ├─ spawn new-api 后端二进制 (electron/resources/bin/new-api[.exe])
  │     └─ Gin HTTP 服务 :3000（前端已嵌入 Go 二进制，buildFS/indexPage）
  └─ BrowserWindow 加载 http://localhost:3000
        └─ Tray 托盘 + 崩溃日志（端口占用 / 数据库锁 / 服务异常诊断）
```

后端静态资源通过 `router.SetRouter(server, router.WebAssets{BuildFS, IndexPage})` 嵌入，
默认端口取 `PORT` env，未设置时用 `common.Port`（默认 3000）——与 Electron 主进程固定值一致。

## 2. 环境要求

| 工具 | 版本要求 | 用途 |
|---|---|---|
| Go | ≥ 1.25.1（CI 使用） | 编译后端二进制 |
| Bun | latest | 前端安装与构建（`web/` 内） |
| Node.js | ≥ 22 | electron-builder / npm |
| npm | 随 Node | electron 依赖安装 |

Windows 额外注意：Go 编译需 CGO（`CGO_ENABLED=1`），需有可用的 C 编译器（MinGW-w64）。

## 3. 构建流程（三阶段）

### 阶段 1：构建前端（产物将嵌入 Go 二进制）

```bash
cd web
bun install --frozen-lockfile
DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION=$(git describe --tags) bun run build
cd ..
```

### 阶段 2：构建 Go 后端二进制

```bash
# Windows
go build -ldflags "-s -w -X 'new-api/common.Version=$(git describe --tags)'" -o new-api.exe

# macOS / Linux
go build -ldflags "-s -w -X 'new-api/common.Version=$(git describe --tags)'" -o new-api
```

> **顺序必须为 前端 → 后端**：前端产物通过 `go:embed` 嵌入二进制，先 build web 再 build Go，
> 否则二进制内是旧版前端。打包前建议先做 `cd relaykit && GOWORK=off go build ./...` 验证子模块。

### 阶段 3：Electron 打包

```bash
cd electron
npm install
npm run build:win        # Windows: nsis + portable
# npm run build:mac      # macOS: dmg + zip（需 macOS 环境）
# npm run build:linux    # Linux: AppImage + deb
```

产物输出到 `electron/dist/`。

## 4. 产物与验证

| 平台 | 产物 | 说明 |
|---|---|---|
| Windows | `New-API-App Setup <ver>.exe` | nsis 安装器（可选安装目录） |
| Windows | `New-API-App <ver>.exe` | portable 免安装版 |
| macOS | `New-API-App-<ver>.dmg` / `.zip` | 需 macOS 构建机 |
| Linux | `*.AppImage` / `*.deb` | AppImage 免安装 / deb 安装包 |

**验证步骤**：

1. 运行 portable exe（或安装后启动）
2. 确认系统托盘出现 new-api 图标
3. 确认主窗口自动打开并加载 `http://localhost:3000`
4. 观察后端日志无 `failed to start HTTP server` / `database is locked` 等错误
5. 若端口被占用，应用应弹出诊断对话框（main.js 内置端口占用检测）

## 5. 三平台差异

- **Windows**：`win.target` = `nsis` + `portable`；`extraResources` 打包 `../new-api.exe` → `bin/new-api.exe`；nsis 配置 `oneClick: false` 允许选择安装目录。
- **macOS**：`mac.target` = `dmg` + `zip`；`identity: null` + `CSC_IDENTITY_AUTO_DISCOVERY: false` 跳过签名；`entitlements.mac.plist` 生效。
- **Linux**：`linux.target` = `AppImage` + `deb`；category `Development`。
- 许可证文件（LICENSE / NOTICE / THIRD-PARTY-LICENSES.md / Electron 自带 LICENSE）随包分发到 `licenses/`。

`electron/build.sh` 按 `$OSTYPE` 自动分支：`darwin` → mac，`linux-gnu` → linux，`msys/cygwin/win32` → win。

## 6. CI 发布（GitHub Actions）

`.github/workflows/electron-build.yml` 触发方式：

- **push tag**：`v*`（排除 `*-*` / `*-alpha*` 预发布 tag）
- **workflow_dispatch**：手动触发

流程：checkout → setup Bun/Node/Go → 构建前端 → 构建 Go（Windows）→
`npm version <tag>` 同步版本 → `npm install` → `npm run build:win` →
上传 `electron/dist/*.exe` artifact → release job 发布到 GitHub Release。

> 注意：当前 workflow 的 matrix 仅启用 `windows-latest`（macOS/Linux 分支被注释），
> 如需三平台发布需在 matrix 中启用 `macos-latest`、`ubuntu-latest` 并放开对应步骤。

## 7. 风险与注意事项

| 风险 | 说明 | 缓解 |
|---|---|---|
| 端口 3000 被占用 | main.js 固定 PORT=3000，冲突时后端启动失败 | 内置诊断弹窗；关闭占用程序或改环境变量 |
| SQLite 数据目录 | 默认数据库文件落在二进制工作目录，portable 模式下可能无写权限 | 设置 `SQL_DSN` 指向 `%APPDATA%`/`~/Library/Application Support` 下路径 |
| 代码签名 | CI 跳过签名（`CSC_IDENTITY_AUTO_DISCOVERY: false`），Windows SmartScreen 会告警 | 生产发布前配置代码签名证书 |
| relaykit 独立编译 | 根模块编译通过不代表 relaykit 可独立构建（AGENTS.md 强制） | 打包前 `cd relaykit && GOWORK=off go build ./...` |
| 前端版本注入 | `VITE_REACT_APP_VERSION` 依赖 git describe，浅克隆需 `fetch-depth: 0` | CI 已配置 fetch-depth: 0 |
| 推送邮箱隐私 | GitHub GH007 会拒绝提交私有邮箱 | 用 `DrewFly@users.noreply.github.com` 推送 |

## 8. 快速参考命令

```bash
# 一键构建（本地脚本，自动按 OS 分支）
./electron/build.sh

# 手动三步
cd web && bun install --frozen-lockfile && DISABLE_ESLINT_PLUGIN='true' bun run build && cd ..
go build -ldflags "-s -w -X 'new-api/common.Version=$(git describe --tags)'" -o new-api.exe
cd electron && npm install && npm run build:win
```
