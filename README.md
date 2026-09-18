# Zebra Code

English | [简体中文](README.zh-CN.md)

Zebra Code is an AI coding agent CLI supporting four run modes: interactive TUI, non-interactive execution, remote web service, and multi-agent collaboration.

## Tech stack

- **Language**: Go 1.25.0
- **TUI**: BubbleTea (Elm architecture) + Lipgloss + Glamour (Markdown rendering)
- **LLM SDKs**: `anthropic-sdk-go` + `openai-go`
- **MCP**: `modelcontextprotocol/go-sdk` (stdio / SSE / Streamable HTTP)
- **Config**: YAML (`gopkg.in/yaml.v3`)

## Code size

- 197 Go files, ~37,200 lines of code

## Run modes

| Mode | Command | Purpose |
|------|---------|---------|
| TUI | `zebracode` | Interactive terminal chat |
| Print | `zebracode -p "prompt"` | Non-interactive, exits after output |
| Remote | `zebracode --remote` | Web server mode |
| Teammate | `zebracode --teammate` | Multi-agent collaboration worker |

## Architecture overview

```
cmd/zebracode/
├── main.go          # Entry: TUI / -p print / --remote / --teammate
├── print.go         # Non-interactive -p mode
├── teammate.go      # Multi-agent collaboration worker mode
└── teammate_test.go

internal/
├── agent/           # Core agent loop (LLM → Tool → result → loop)
├── llm/             # LLM clients (Anthropic / OpenAI / OpenAI-compat)
├── tools/           # Built-in tools (ReadFile, WriteFile, EditFile, Bash, Glob, Grep, ...)
├── tui/             # BubbleTea TUI (chat, slash commands, permission dialogs)
├── conversation/    # Conversation history manager
├── config/          # YAML config loading (Provider / MCP / Sandbox)
├── permissions/     # Multi-layer permission system (path sandbox + rule engine + 4 modes)
├── sandbox/         # OS-level sandbox (macOS seatbelt / Linux bubblewrap)
├── mcp/             # MCP protocol client (stdio / SSE / Streamable HTTP)
├── skills/          # Skill system (SKILL.md loading, inline/fork execution)
├── agents/          # Sub-agent tools (AgentTool, definition loading, tool filtering)
├── teams/           # Multi-agent collaboration (team create/destroy, mailbox, coordinator mode)
├── memory/          # Long-term memory (auto-extraction, relevance recall, consolidation)
├── hooks/           # Lifecycle hooks (before/after session/turn/tool; command/prompt/http/agent)
├── compact/         # Context compaction (auto + forced, with RecoveryState)
├── session/         # Session persistence (disk log, resume)
├── filehistory/     # File snapshot history (rewind)
├── prompt/          # System prompt builder (environment detection, Plan/Coordinator guidance)
├── commands/        # Slash command registry
├── todo/            # Task list tool
├── planfile/        # Plan Mode file management
├── worktree/        # Git worktree isolation (enter/exit/cleanup)
├── history/         # Input history
├── toolresult/      # Tool result spill / budget management
└── remote/          # Remote server mode (web UI)
```

## Core design

### 1. Agent loop

```
LLM stream → collect TextDelta/ToolCall → run tools → feed results back → loop until no tool calls
```

- **Streaming**: LLM output is pushed live to a `chan AgentEvent`
- **Tool execution**: `StreamingExecutor` batches by safety — read-only tools run concurrently, writes and commands run serially
- **Auto-recovery**: on `max_tokens`, automatic escalation + multi-round recovery (up to 3)
- **Context management**: Layer 1 (tool-result budget spill) + Layer 2 (auto compaction)

### 2. Multi-protocol LLM

- A single `Client` interface with a unified `Stream()` method
- Three implementations: `anthropic.go`, `openai.go`, `openai_compat.go`
- Context window resolved in four layers: config → runtime fetch → model mapping table → default

### 3. Tool system

- `Tool` interface: `Name / Description / Category / Schema / Execute`
- 4 categories: `Read` / `Write` / `Command`
- `DeferrableTool`: MCP tools are lazy-loaded by default and discovered on demand via `ToolSearch`
- Spill mechanism: a single result over 50K chars is written to disk in exchange for a preview; an aggregate budget prevents whole-batch overflow

### 4. Permission system

- 4-layer check: dangerous-command detection → path sandbox → rule engine → mode matrix
- 4 modes:
  - `default`: reads allowed / writes ask / commands ask
  - `acceptEdits`: reads and writes allowed / commands ask
  - `plan`: read-only; output goes to a plan file and runs after user approval
  - `bypass`: everything allowed

### 5. Multi-agent collaboration

- **Team model**: a Lead plus multiple Teammates communicating through a file mailbox
- **Coordinator mode**: the Lead only dispatches and does not write code; Teammates execute
- Execution backends: tmux / iTerm / in-process
- Shared tasks (`SharedTask`): parallel subtasks + result aggregation

### 6. Skill system

- YAML frontmatter + Markdown body
- Two execution modes: `inline` (inject into the current conversation) / `fork` (isolated sub-agent)
- Install from URL (skills.sh / GitHub / raw URL)
- `$ARGUMENTS` template variable

### 7. Memory system

- **Auto-extraction**: `OnLoopComplete` triggers background extraction of key facts from the conversation
- **Relevance recall**: each turn prefetches relevant memories into a system reminder
- **Consolidation**: background merging of duplicates, removal of stale entries, correction of contradictions

### 8. Context compaction

- **Layer 1**: tool results spill to disk by budget when entering history
- **Layer 2**: token usage is auto-detected to trigger LLM summarization
- **RecoveryState**: after compaction, file reads and skill invocations are retained and the context is rebuilt on recovery

## Configuration

See `.zebracode/config.yaml.example`:

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

## Code conventions

- Commit messages in English
- Variable names use snake_case

## Highlights

1. **Streaming tool execution**: tool calls are collected while the LLM is still streaming and executed in safety batches once the stream ends
2. **Tool-result spill**: keeps oversized tool output from blowing up the context window by writing to disk in exchange for a preview
3. **Coordinator mode**: narrows the Lead's toolset dynamically via `ToolNameFilter`, with no agent restart
4. **Skill fork mode**: isolated sub-agent execution with full/recent/none context inheritance strategies
5. **Memory prefetch**: non-blocking memory recall running in parallel with the main LLM call
