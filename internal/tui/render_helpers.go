package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ericthz/zebra-code/internal/permissions"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var menuActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))

func (m Model) renderSlashMenu() string {
	var sb strings.Builder
	for i, cmd := range m.slashMatches {
		desc := cmd.Description
		if len([]rune(desc)) > 30 {
			desc = string([]rune(desc)[:28]) + "…"
		}
		label := fmt.Sprintf("  /%-16s — %s", cmd.Name, desc)
		if i == m.slashCursor {
			sb.WriteString(menuActiveStyle.Render(label) + "\n")
		} else {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render(label) + "\n")
		}
	}
	return sb.String()
}

func (m Model) renderAtMenu() string {
	var sb strings.Builder
	for i, path := range m.atMatches {
		if i == m.atCursor {
			sb.WriteString(menuActiveStyle.Render("  "+path) + "\n")
		} else {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  "+path) + "\n")
		}
	}
	return sb.String()
}

func (m Model) renderPermDialog() string {
	var sb strings.Builder

	// Command header
	sb.WriteString(permBorderStyle.Render(fmt.Sprintf("  %s command", m.permToolName)))
	sb.WriteString("\n\n")

	// Command detail
	desc := m.permDesc
	if desc != "" {
		sb.WriteString(lipgloss.NewStyle().Foreground(normalText).PaddingLeft(4).Render(desc))
		sb.WriteString("\n\n")
	}

	// Approval notice
	sb.WriteString(permDimStyle.Render("  This command requires approval"))
	sb.WriteString("\n\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(normalText).PaddingLeft(2).Render("Do you want to proceed?"))
	sb.WriteString("\n")

	// Selectable options
	for i, opt := range permOptions {
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.permCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		label := fmt.Sprintf("%d. %s", i+1, opt.label)
		sb.WriteString(style.Render(prefix+label) + "\n")
	}

	sb.WriteString("\n")
	return sb.String()
}

func (m Model) renderRewindDialog() string {
	if m.rewindPhase == 1 {
		return m.renderRewindOptionsDialog()
	}
	return m.renderRewindSnapshotList()
}

func (m Model) renderRewindSnapshotList() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render("  ⟲ Rewind to checkpoint"))
	sb.WriteString("\n\n")

	maxVisible := 8
	start := 0
	if m.rewindCursor >= maxVisible {
		start = m.rewindCursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(m.rewindSnapshots) {
		end = len(m.rewindSnapshots)
	}

	for i := start; i < end; i++ {
		snap := m.rewindSnapshots[i]
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.rewindCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		ago := time.Since(snap.Timestamp).Truncate(time.Second)
		label := snap.UserText
		if len(label) > 50 {
			label = label[:50] + "…"
		}
		files := len(snap.Backups)
		line := fmt.Sprintf("%s[%d] %s (%s ago, %d file(s))", prefix, i+1, label, ago, files)
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render("↑/↓ navigate · enter select · esc cancel"))
	sb.WriteString("\n")
	return sb.String()
}

func (m Model) renderRewindOptionsDialog() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render("  ⟲ Rewind to checkpoint"))
	sb.WriteString("\n\n")

	snap := m.rewindSnapshots[m.rewindCursor]
	ago := time.Since(snap.Timestamp).Truncate(time.Second)
	label := snap.UserText
	if len(label) > 50 {
		label = label[:50] + "…"
	}
	selected := fmt.Sprintf("  Selected: [%d] %s (%s ago, %d file(s))", m.rewindCursor+1, label, ago, len(snap.Backups))
	sb.WriteString(lipgloss.NewStyle().Foreground(normalText).Render(selected))
	sb.WriteString("\n\n")

	for i, opt := range rewindOptions {
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.rewindOptionCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		sb.WriteString(style.Render(prefix+opt) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render("↑/↓ navigate · enter select · esc back"))
	sb.WriteString("\n")
	return sb.String()
}

func (m Model) renderMarkdown(content string) string {
	// Don't use WithAutoStyle — it queries the terminal background via OSC 11
	// every time, and the response leaks into stdin and pollutes the input.
	// Force TrueColor explicitly: without a profile, glamour delegates to
	// termenv auto-detection, which fails under bubbletea's stdin takeover
	// and falls back to the no-color "notty" style — markdown then prints raw.
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithColorProfile(termenv.TrueColor),
		glamour.WithWordWrap(m.width-6),
	)
	if err != nil {
		return content
	}
	rendered, err := r.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimSpace(rendered)
}

// ── Helpers ──

func toolTitle(toolName string, args map[string]any) string {
	switch toolName {
	case "ReadFile":
		p, _ := args["file_path"].(string)
		if p != "" {
			return "Read " + filepath.Base(p)
		}
		return "Read"
	case "WriteFile":
		p, _ := args["file_path"].(string)
		content, _ := args["content"].(string)
		lines := strings.Count(content, "\n") + 1
		if p != "" {
			return fmt.Sprintf("Write %s (%d lines)", filepath.Base(p), lines)
		}
		return "Write"
	case "EditFile":
		p, _ := args["file_path"].(string)
		if p != "" {
			return "Edit " + filepath.Base(p)
		}
		return "Edit"
	case "Bash":
		cmd, _ := args["command"].(string)
		if len(cmd) > 50 {
			cmd = cmd[:50] + "…"
		}
		if cmd != "" {
			return fmt.Sprintf("Bash: %s", cmd)
		}
		return "Bash"
	case "Glob":
		pattern, _ := args["pattern"].(string)
		return fmt.Sprintf("Glob: %s", pattern)
	case "Grep":
		pattern, _ := args["pattern"].(string)
		return fmt.Sprintf("Grep: %s", pattern)
	}
	return toolName
}

func indentBlock(text string, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) pastTense(verb string) string {
	if strings.HasSuffix(verb, "ing") {
		stem := strings.TrimSuffix(verb, "ing")
		if strings.HasSuffix(stem, "at") || strings.HasSuffix(stem, "ut") ||
			strings.HasSuffix(stem, "it") || strings.HasSuffix(stem, "et") {
			return stem + "ed"
		}
		if strings.HasSuffix(stem, "e") {
			return stem + "d"
		}
		return stem + "ed"
	}
	return verb
}

func (m Model) getModelName() string {
	if m.selectedProvider != nil {
		return m.selectedProvider.Model
	}
	if len(m.providers) > 0 {
		return m.providers[0].Model
	}
	return "unknown"
}

func (m Model) getWorkDir() string {
	wd, _ := os.Getwd()
	return wd
}

func nextPermissionMode(current permissions.PermissionMode) permissions.PermissionMode {
	switch current {
	case permissions.ModeDefault:
		return permissions.ModeAcceptEdits
	case permissions.ModeAcceptEdits:
		return permissions.ModePlan
	case permissions.ModePlan:
		return permissions.ModeBypass
	case permissions.ModeBypass:
		return permissions.ModeDefault
	default:
		return permissions.ModeDefault
	}
}
