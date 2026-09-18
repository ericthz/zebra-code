# Zebra Code Project Instructions

Zebra Code is an AI coding agent CLI written in Go (an independent implementation in the spirit of Claude Code). It supports four run modes: interactive TUI, non-interactive execution, remote web service, and multi-agent collaboration.

This is the cross-tool project instruction file (the AGENTS.md standard). It is auto-loaded at session start by zebra-code, Codex, opencode, Cursor, and other tools that support AGENTS.md; Claude Code reads it through the `@AGENTS.md` import in `CLAUDE.md`. **Edits here change the context injected into future agent sessions.**

## Quick facts
- Language: Go 1.25.0
- Go module: `github.com/ericthz/zebra-code`
- Entry point: `cmd/zebracode/main.go`
- Build artifact: the `zebracode` binary at the repo root (git-ignored)

## Build and test
- Build: `go build ./cmd/zebracode`
- All tests: `go test ./...`
- Single package: `go test ./internal/<pkg>`
- Static checks: `go vet ./...`
- Format: `gofmt -w .` (keep the tree gofmt-clean before committing)
- Run locally: `go run ./cmd/zebracode` (or run the built binary)

## Code conventions
- Commit messages in English; one focused change per commit
- Variable names use snake_case
- Follow the existing layering: `cmd/` for CLI entry points, `internal/` for implementation
- Do not commit build artifacts, generated files, or runtime data (see below)

## Layout
- `cmd/zebracode/`: CLI entry (`main.go`; `print.go` for non-interactive `-p` mode; `teammate.go` for the collaboration worker)
- `internal/`: subsystems
  - `agent/` core agent loop; `llm/` LLM clients (Anthropic / OpenAI / OpenAI-compat)
  - `tools/` built-in tools; `permissions/` permissions and dangerous-command detection; `sandbox/` sandboxing
  - `tui/` BubbleTea terminal UI (split by responsibility: `update.go`, `dialogs.go`, `render_chat.go`, `render_tools.go`, `agent_setup.go`, `resume_rewind.go`, `model.go`, ...)
  - `conversation/` history; `compact/` context compaction; `memory/` long-term memory
  - `teams/` multi-agent collaboration; `remote/` remote web service; `skills/`, `hooks/`, `commands/`, `agents/`, ...
- `docs/`: shared documents pushed to the remote (committed and pushed)
- `docs/personal/`: local personal documents, git-ignored and never pushed (drafts / private notes)

## Local and runtime data (important — do not commit)
- `.zebracode/`: runtime and user data, git-ignored
  - `config.yaml` holds API keys; `sessions/`, `memory/`, `file-history/` hold full conversation logs and edited-file snapshots — private
  - Never commit real secrets; only `config.yaml.example` is tracked as a template
  - To stop tracking an already-tracked file, use `git rm --cached <path>`
- `.workbuddy/`: WorkBuddy agent internal state (agent memory and private data), git-ignored

## Operational cautions
- `--remote` runs a Web/WebSocket server bound to `:18888` by default with **no authentication and no origin check** — never expose it publicly
- The command-execution sandbox is **off by default**; the Bash tool relies on a command blacklist plus path-prefix checks, so run untrusted commands with care

## Naming convention
- **`Zebra Code`**: the product name — use in prose, docs, code comments, and user-facing strings (TUI banner, dialogs, web title, system prompts).
- **`zebracode`** (all lowercase): commands and identifiers — the binary/command, paths (`cmd/zebracode`), config dir (`.zebracode/`), env namespace (`~/.zebracode`), code identifiers, log/MCP identifiers.
- **`zebra-code`** (hyphenated): only the Go module path and repo URL (`github.com/ericthz/zebra-code`) — structural, do not rename.
- Do not use `Zebracode`, `ZebraCode` (camelCase) or `ZEBRACODE` (all caps; only the legacy filename `ZEBRACODE.md` used it).
- The project instruction file is **`AGENTS.md`** (the old `ZEBRACODE.md` is deprecated; `CLAUDE.md` is only a Claude Code import bridge).
