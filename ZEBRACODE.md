# Zebracode 项目

zebracode 是一个用 Go 实现的 AI 编程 Agent CLI（类似 Claude Code 的自主实现）。

## 技术栈
- Go（模块名 `zebracode`）

## 构建与测试
- 构建：`go build ./cmd/zebracode`（产物二进制 `zebracode` 生成在仓库根，已被 `.gitignore` 忽略）
- 测试：`go test ./...`
- 静态检查：`go vet ./...`
- 本地运行：`go run ./cmd/zebracode`（或直接使用构建出的二进制）

## 代码规范
- commit message 使用英文
- 变量命名使用 snake_case
- 新增包/功能遵循现有分层：`cmd/` 放 CLI 入口，`internal/` 放实现

## 目录约定
- `cmd/zebracode/`：CLI 入口（main、teammate、print 等）
- `internal/`：各子系统，例如 agent 主循环、permissions 权限、sandbox 沙箱、llm、tools、tui、teams 多 Agent 协作、remote Web 服务、memory 长期记忆、compact 上下文压缩等
- `docs/`：可上传远端的共享文档（提交并推送）
- `docs/personal/`：本地个人文档，已被 `.gitignore` 忽略，不上传远端（草稿/隐私用）

## 本地/运行时数据（重要）
- `.zebracode/` 存放运行时与用户数据，已被 `.gitignore` 忽略：
  - `config.yaml` 含 API key 等密钥；`sessions/`、`memory/`、`file-history/` 含对话全文、被编辑文件快照与私密信息
  - 切勿提交真实密钥；仅 `config.yaml.example` 作为模板被跟踪
  - 若需把已跟踪的文件移出版本库，用 `git rm --cached <path>`
- `.workbuddy/` 是 WorkBuddy 代理内部状态（含代理记忆与隐私），已被忽略

## 运行注意事项
- 存在 `--remote` 模式（Web/WebSocket 服务，默认监听 `:18888`，无鉴权），切勿在公网/暴露环境开启
- 命令执行沙箱默认关闭，Bash 工具依赖命令黑名单与路径前缀做防护，执行不明命令须谨慎

## 说明
本文件会被 zebracode 在会话启动时自动加载为项目指令（类似 `CLAUDE.md`），用于向 Agent 注入项目上下文；改动会影响后续会话的自动注入内容。
