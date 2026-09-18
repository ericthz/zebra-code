# Zebra Code

[English](README.md) | 简体中文

Zebra Code 是 AI 编程 Agent CLI 工具，支持交互式 TUI、非交互式执行、远程 Web 服务、多 Agent 协作四种运行模式。

## 技术栈

- **语言**：Go 1.25.0
- **TUI**：BubbleTea (Elm 架构) + Lipgloss + Glamour (Markdown 渲染)
- **LLM SDK**：`anthropic-sdk-go` + `openai-go`
- **MCP**：`modelcontextprotocol/go-sdk`（stdio / SSE / Streamable HTTP）
- **配置**：YAML (`gopkg.in/yaml.v3`)

## 代码规模

- 197 个 Go 文件，约 37,200 行代码

## 运行模式

| 模式 | 命令 | 用途 |
|------|------|------|
| TUI | `zebracode` | 交互式终端聊天 |
| Print | `zebracode -p "prompt"` | 非交互式，输出后退出 |
| Remote | `zebracode --remote` | Web 服务器模式 |
| Teammate | `zebracode --teammate` | 多 Agent 协作 worker |

## 架构总览

```
cmd/zebracode/
├── main.go          # 入口：TUI / -p print / --remote / --teammate 四种模式
├── print.go         # 非交互式 -p 模式
├── teammate.go      # 多 Agent 协作 worker 模式
└── teammate_test.go

internal/
├── agent/           # 核心 Agent 循环（LLM → Tool → 结果 → 循环）
├── llm/             # LLM 客户端（Anthropic / OpenAI / OpenAI-compat）
├── tools/           # 内建工具（ReadFile, WriteFile, EditFile, Bash, Glob, Grep 等）
├── tui/             # BubbleTea TUI 界面（聊天、slash 命令、权限对话框）
├── conversation/    # 对话历史管理器
├── config/          # YAML 配置加载（Provider / MCP / Sandbox）
├── permissions/     # 多层权限系统（路径沙箱 + 规则引擎 + 4 种模式）
├── sandbox/         # OS 级沙箱（macOS seatbelt / Linux bubblewrap）
├── mcp/             # MCP 协议客户端（stdio / SSE / Streamable HTTP）
├── skills/          # Skill 系统（SKILL.md 加载、inline/fork 两种执行模式）
├── agents/          # 子 Agent 工具（AgentTool、定义加载、工具过滤）
├── teams/           # 多 Agent 协作（Team 创建/销毁、消息邮箱、Coordinator 模式）
├── memory/          # 长期记忆（自动提取、相关性召回、整理合并）
├── hooks/           # 生命周期钩子（session/turn/tool 前后，支持 command/prompt/http/agent）
├── compact/         # 上下文压缩（自动管理 + 强制压缩 + RecoveryState 恢复）
├── session/         # 会话持久化（磁盘日志、恢复）
├── filehistory/     # 文件快照历史（rewind 恢复）
├── prompt/          # 系统提示词构建（环境检测、Plan/Coordinator 指引）
├── commands/        # Slash 命令注册表
├── todo/            # 任务清单工具
├── planfile/        # Plan Mode 文件管理
├── worktree/        # Git Worktree 隔离（enter/exit/cleanup）
├── history/         # 输入历史
├── toolresult/      # 工具结果溢写/预算管理
└── remote/          # 远程服务器模式（Web UI）
```

## 核心设计模式

### 1. Agent Loop

```
LLM Stream → 收集 TextDelta/ToolCall → 执行工具 → 结果回传 → 循环直到无工具调用
```

- **流式处理**：LLM 输出实时推送到 `chan AgentEvent`
- **工具执行**：`StreamingExecutor` 按安全性分批——只读并发、写/命令串行
- **自动恢复**：`max_tokens` 时自动 escalation + 多轮 recovery（最多 3 次）
- **上下文管理**：Layer 1（工具结果预算溢写）+ Layer 2（自动压缩）

### 2. 多协议 LLM

- `Client` 接口统一 `Stream()` 方法
- 三种实现：`anthropic.go`、`openai.go`、`openai_compat.go`
- Context Window 四层解析：config → 运行时 fetch → 模型映射表 → 默认值

### 3. 工具系统

- `Tool` 接口：`Name / Description / Category / Schema / Execute`
- 4 种分类：`Read` / `Write` / `Command`
- `DeferrableTool` 接口：MCP 工具默认延迟加载，用 `ToolSearch` 按需发现
- 溢写机制：单条 >50K 字符写盘换预览，聚合预算防止整批超限

### 4. 权限系统

- 4 层检查：危险命令检测 → 路径沙箱 → 规则引擎 → 模式矩阵
- 4 种模式：
  - `default`：读放行 / 写问 / 命令问
  - `acceptEdits`：读写放行 / 命令问
  - `plan`：只读模式，输出到 plan 文件，用户审批后执行
  - `bypass`：全部放行

### 5. 多 Agent 协作

- **Team 模型**：Lead + 多个 Teammate，通过文件邮箱通信
- **Coordinator 模式**：Lead 只调度不写代码，Teammate 各自执行
- 执行后端：tmux / iTerm / in-process
- 共享任务（`SharedTask`）：并行子任务 + 结果聚合

### 6. Skill 系统

- YAML frontmatter + Markdown body 格式
- 两种执行模式：`inline`（注入当前对话）/ `fork`（隔离子 Agent）
- 支持从 URL 安装（skills.sh / GitHub / raw URL）
- `$ARGUMENTS` 模板变量

### 7. 记忆系统

- **自动提取**：`OnLoopComplete` 触发后台提取对话中的关键信息
- **相关性召回**：每轮 prefetch 相关记忆注入 system-reminder
- **整理合并**：后台合并重复、删除过时、修正矛盾

### 8. 上下文压缩

- **Layer 1**：工具结果入历史时按预算溢写
- **Layer 2**：自动检测 token 用量，触发 LLM 生成摘要压缩
- **RecoveryState**：压缩后保留文件读取和 Skill 调用记录，恢复时重建上下文

## 配置

参考 `.zebracode/config.yaml.example`：

```yaml
providers:
  - name: anthropic-official
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: "your-api-key-here"
    model: claude-sonnet-4-20250514
    thinking: true

permission_mode: default

mcp_servers:
  - name: context7
    command: npx
    args: ["-y", "@upstash/context7-mcp"]

enable_coordinator_mode: false
```

## 代码规范

- commit message 用英文
- 变量命名用 snake_case

## 项目亮点

1. **流式工具执行**：在 LLM 流式输出期间就开始收集工具调用，流结束后按安全性分批执行
2. **工具结果溢写**：防止超大工具输出撑爆上下文窗口，自动写盘换预览
3. **Coordinator 模式**：通过 `ToolNameFilter` 动态收窄 Lead 的工具集，无需重启 Agent
4. **Skill fork 模式**：子 Agent 隔离执行，支持 full/recent/none 三种上下文继承策略
5. **Memory prefetch**：与主 LLM 调用并行的非阻塞记忆召回
