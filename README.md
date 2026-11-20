# edualc

轻量、跨平台（**macOS / Windows / Linux**）的 **Claude 隔离工作区与本地订阅中转平台**。

将官方 **Claude Desktop**、**Claude Code CLI** 与 **独立隔离 Chromium 浏览器** 全部封装在无公网网卡的 Docker 隔离内网中，同时通过 **宿主机透明命令代理** 让容器内的 Claude 直接调用宿主机原生 CPU / GPU / SSD 工具链（`cargo`、`git`、`bun`、`just`、`rg` 等），兼顾 **零网络/环境泄露** 与 **原生开发速度**。

---

## 核心特性

### 1. 10 项 Fail-Closed 防泄露安全门控
任一关键项失败或过期即自动熔断拒绝发包，支持 3 次退避重试与每 20 秒后台自动自愈：
1. **直连公网 TCP 阻断**：`relay` 与 `desktop` 容器位于 `internal: true` 隔离内网，物理无公网路由。
2. **UDP 出站阻断**：彻底拦截公网 UDP 与 WebRTC 真实 IP 泄露。
3. **IPv6 出站阻断**：防止双栈网络旁路泄露。
4. **代理故障断网（零 `DIRECT`）**：自动清洗导入的 Clash / Mihomo 订阅，剥离所有 `DIRECT` 规则，禁用 IPv6。
5. **代理通道连通性**：严格经代理获取真实公网 IPv4（拒绝重定向与私网地址）。
6. **DNS 远端解析隔离**：阻断本地明文 UDP 53，域名全程经代理远端解析。
7. **出口 IP 与地区风控**：内置离线中国大陆 IPv4 网段库 + 多源地理定位校验，拦截受限地区，支持锁定国家 / 州省 / 城市 / 固定静态 IP。
8. **时区与语言环境同步**：自动按出口 IP 同步 `TZ`、`/etc/localtime` 与 `LANG`，注入 CLI、Desktop 与隔离浏览器。
9. **宿主机文件系统隔离**：验证未误挂载宿主机 `~/.ssh` 等敏感目录。
10. **Anthropic 出口连通性与实时延迟**：实时检测 `api.anthropic.com` TLS 连通与延迟（ms）。

### 2. 全家桶隔离运行环境（全同源 Web 控制台）
打开 `http://127.0.0.1:8317/ui` 即可一站式使用：
- **出口与订阅管理**：支持 Clash/Mihomo 订阅导入、节点批量测速切换、静态 SOCKS5/HTTP、以及 **链式代理（机场前置 → 静态住宅 IP 落地）**。
- **官方 Claude Desktop**（独立容器 `desktop`，与 `relay` 网络解耦，更新服务不掉线）：支持双向 UTF-8 剪贴板、Mac/Win 快捷键、宿主机原生中文输入条（`Option/Alt + Space`）、Desktop 外部链接自动跳转隔离浏览器。
- **独立隔离 Chromium 浏览器**：同出口 IP、同步时区与 `Accept-Language`、禁用非代理 WebRTC UDP，安全完成 OAuth 授权与网页使用。
- **容器终端**：内置 Web 终端（`ttyd`）与本机直连 CLI `edualc`。
- **多账号池与零 Token 用量查询**：每账号独立 `CLAUDE_CONFIG_DIR`，429 自动冷却切号，实时查看 5 小时 / 7 天限额窗口。
- **OpenAI & Anthropic 本地兼容接口**：通过容器内正版 `claude` CLI 子进程流式转发，适配 Cherry Studio / SDK。

### 3. 宿主机透明命令代理（`hostexec` + `hostshim` + `cmdhook`）
- 容器内执行白名单工具（如 `cargo`、`rustc`、`rustup`、`git`、`just`、`make`、`bun`、`rg`、`codex`、`agy` 等）时，自动穿透到宿主机原生环境执行，并将输出路径脱敏还原为容器内路径。
- **防卡死与并发安全**：`git` 写操作串行互斥锁（杜绝 `.git/index.lock` 冲突）、进程组 PGID 级联清理、环境变量与 `stdin` 完整透传、共享临时目录 `/tmp/edualc`。
- **动态热增工具**：内置 Hook 自动捕获容器内未代理或缺失命令，控制台支持一键加入白名单，免重启立即生效。

### 4. 批量续跑与定时自动续跑
- 自动扫描近期 Claude Desktop 会话转录文件，精准区分 **运行中（`ACTIVE`，45 秒内仍在写入）**、**中断待续（`STALLED`，停在 `tool_use` / `tool_result` / `thinking` / 未回复用户消息或中断错误）** 与 **已完成（`DONE`，已有最终助手回复）**。
- 支持 Web UI 一键续跑、单条续跑、命令行 `edualc resume --all`，以及 **定时自动续跑**（可配 5~60 分钟巡检周期、单状态最大重试上限防死循环、自动等待 10/10 绿灯），续跑结果实时同步回 Claude Desktop 界面。

---

## 架构概览

```text
[宿主机：macOS / Windows / Linux]
  浏览器控制台 (http://127.0.0.1:8317/ui)  ·  edualc CLI  ·  hostexec 守护进程 (:8325)
         │                                                       ▲
         ▼                                                       │ 白名单命令透传 + 路径脱敏
┌────────────────────────────────────────────────────────────┐   │
│ gateway (public_net + cage_internal)                       │───┘
│  • Mihomo 代理核心（零 DIRECT 清洗 / 链式落地 / 节点测速）    │
│  • 统一同源反向代理 (:8317) + Desktop (:8318) + Term (:8319) │
└──────────────┬───────────────────────────────┬─────────────┘
               │ cage_internal (internal:true) │ cage_internal (internal:true)
               ▼                               ▼
┌──────────────────────────────┐  ┌─────────────────────────────────────┐
│ relay (无公网网卡)            │  │ desktop (独立网络，与 relay 解耦)     │
│  • 10 项 Fail-Closed 安全引擎 │  │  • 官方 Claude Desktop (Display :78) │
│  • Web UI + OpenAI/Anthropic │  │  • 独立隔离 Chromium (Display :79)   │
│  • 多账号池 + 用量查询 + ttyd │  │  • UTF-8 剪贴板桥 + 原生中文输入条    │
└──────────────────────────────┘  └─────────────────────────────────────┘
```

---

## 快速开始

### 前置依赖
- **macOS**：Docker 运行时（推荐 [Colima](https://github.com/abiosoft/colima) 或 OrbStack / Docker Desktop）+ Go 1.22+（用于编译宿主机 `hostexec`）+ Python 3
- **Windows**：Docker Desktop（WSL2 后端）+ Go 1.22+ + Python 3
- **Linux**：Docker Engine + Docker Compose v2 + Go 1.22+ + Python 3

### 1. 克隆并初始化配置
```bash
git clone https://github.com/droog126/edualc.git
cd edualc
cp .env.example .env
```
按需编辑 `.env` 中的 `WORKSPACE_DIR`（指向你想在容器内开发的单个宿主机项目目录，切勿挂载家目录或 `~/.ssh`）与 `WORKSPACE_NAME`。

### 2. 一键启动
- **macOS / Linux**：
  ```bash
  ./scripts/start.sh
  ```
- **Windows (PowerShell)**：
  ```powershell
  .\scripts\start.ps1
  ```

### 3. 配置出口并完成隔离登录
1. 打开控制台 **http://127.0.0.1:8317/ui** → **「出口与订阅」**，导入 Clash/Mihomo 订阅（推荐配合「链式落地静态住宅 IP」）或静态代理，选择海外节点。
2. 确认状态变为 **10/10 绿灯放行** 后，点击 **「绑定当前出口」** 锁死地区防漂移。
3. 登录账号（**严禁在宿主机日常浏览器登录**）：
   - **Claude Desktop**：进入控制台 **「Claude 桌面端」** 页直接登录，授权链接会自动在隔离 Chromium 中打开。
   - **Claude CLI**：在控制台 **「容器终端」** 运行 `claude auth login`，或在本机运行 `./scripts/login.sh default`（Windows：`.\scripts\login.ps1 -Account default`）。

### 4. 日常命令与客户端接入
可将 `scripts/edualc` 软链到 `~/.local/bin/edualc`（Windows 可将 `scripts/` 加入 `PATH`）：
```bash
edualc status               # 查看 10 项安全门控、出口 IP、延迟与账号池
edualc                      # 进入容器 /workspace 交互终端
edualc claude               # 在容器内运行官方 claude CLI（门控红灯自动拦截）
edualc browser [URL]        # 在隔离 Chromium 中打开网址
edualc resume --list        # 扫描中断的 Claude Desktop 会话
edualc resume --all         # 批量续跑全部中断会话
```

**第三方客户端接入（如 Cherry Studio / OpenAI SDK / Anthropic SDK）：**
- OpenAI Base URL：`http://127.0.0.1:8317/v1`
- Anthropic Base URL：`http://127.0.0.1:8317`
- API Key：本机免密时填任意占位符（如 `sk-local`），或使用 `.env` 中设置的 `RELAY_API_KEY`
- 模型别名：`sonnet` / `opus` / `haiku`

---

## 更多文档

- [防封与环境隔离实操指南](docs/ANTI_BAN_GUIDE.md)

## 免责声明与致谢

- 本项目采用 [MIT License](LICENSE) 开源，仅限个人合法合规自用与学习研究；请遵守服务商使用条款。
- 离线中国大陆 IPv4 网段表参考自 MIT 许可的开源项目。
