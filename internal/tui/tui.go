package tui

// tui.go previously contained the entire TUI implementation (~4451 lines).
// It has been split into focused files in this package:
//   model.go, lifecycle.go, update.go, agent_setup.go, streaming.go,
//   input_chat.go, commands.go, dialogs.go, events.go, render_tools.go,
//   render_chat.go, render_helpers.go, resume_rewind.go, history.go
// See docs/personal/TUI_SPLIT_PLAN.md for the layout and rationale.
