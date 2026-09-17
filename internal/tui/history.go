package tui

import (
	"github.com/ericthz/zebra-code/internal/permissions"
)

func (m *Model) historyUp() {
	if len(m.historyEntries) == 0 {
		return
	}
	if m.historyIndex == 0 {
		m.historyDraft = m.textarea.Value()
	}
	if m.historyIndex < len(m.historyEntries) {
		m.historyIndex++
		m.textarea.Reset()
		m.textarea.SetHeight(1)
		m.textarea.SetValue(m.historyEntries[len(m.historyEntries)-m.historyIndex])
	}
}

func (m *Model) historyDown() {
	if m.historyIndex <= 0 {
		return
	}
	m.historyIndex--
	m.textarea.Reset()
	m.textarea.SetHeight(1)
	if m.historyIndex == 0 {
		m.textarea.SetValue(m.historyDraft)
	} else {
		m.textarea.SetValue(m.historyEntries[len(m.historyEntries)-m.historyIndex])
	}
}

func permissionModeInfo(mode permissions.PermissionMode) (string, string) {
	switch mode {
	case permissions.ModeDefault:
		return "Default", "Writes and commands require approval."
	case permissions.ModeAcceptEdits:
		return "Accept Edits", "File edits auto-approved; commands still require approval."
	case permissions.ModePlan:
		return "Plan", "Read-only mode. No writes or commands executed."
	case permissions.ModeBypass:
		return "YOLO", "All tools auto-approved. Use with caution."
	default:
		return string(mode), ""
	}
}
