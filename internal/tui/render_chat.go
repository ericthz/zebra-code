package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/teams"

	"github.com/charmbracelet/lipgloss"
)

func (m Model) View() string {
	if !m.ready {
		return ""
	}

	switch m.state {
	case stateProviderSelect:
		return m.renderProviderSelectView()
	case stateChat:
		return m.renderChatView()
	case stateResume:
		return m.renderResumeView()
	}
	return ""
}

func (m Model) renderProviderSelectView() string {
	var sb strings.Builder

	sb.WriteString(m.renderBanner())
	sb.WriteString("\n\n")

	sb.WriteString(selectLabelStyle.Render("Select a Provider"))
	sb.WriteString("\n\n")

	for i, p := range m.providers {
		if i == m.providerCursor {
			sb.WriteString(selectedItemStyle.Render(fmt.Sprintf("  ❯ %s  [%s]", p.Name, p.Model)))
		} else {
			sb.WriteString(normalItemStyle.Render(fmt.Sprintf("    %s  [%s]", p.Name, p.Model)))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m Model) renderTeammateTree() string {
	if m.teamMgr == nil {
		return ""
	}
	progressList := m.teamMgr.GetAllTeammateProgress()
	if len(progressList) == 0 {
		return ""
	}

	cyanStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00d7ff"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff00"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ffff00"))

	var sb strings.Builder
	sb.WriteString("\n")

	// Leader line
	sb.WriteString("  ┌─ ")
	sb.WriteString(cyanStyle.Render("team-lead"))
	sb.WriteString(": ")
	sb.WriteString(dimStyle.Render(m.thinkingVerb + "…"))
	if m.totalInput+m.totalOutput > 0 {
		sb.WriteString(dimStyle.Render(fmt.Sprintf(" · %s tokens", teams.FormatTokens(int64(m.totalInput+m.totalOutput)))))
	}
	sb.WriteString("\n")

	// Teammate lines
	for i, p := range progressList {
		isLast := i == len(progressList)-1
		connector := "  ├─ "
		if isLast {
			connector = "  └─ "
		}

		sb.WriteString(connector)
		sb.WriteString(cyanStyle.Render("@" + p.Name))
		sb.WriteString(": ")

		status := p.GetStatus()
		switch status {
		case "completed":
			sb.WriteString(greenStyle.Render("completed"))
		case "failed":
			sb.WriteString(redStyle.Render("failed"))
		case "stopped":
			sb.WriteString(yellowStyle.Render("stopped"))
		case "idle":
			sb.WriteString(dimStyle.Render("idle"))
		default:
			sb.WriteString(dimStyle.Render(p.ActivitySummary() + "..."))
		}

		stats := fmt.Sprintf(" · %d tools · %s tokens", p.GetToolUseCount(), teams.FormatTokens(p.GetTokenCount()))
		sb.WriteString(dimStyle.Render(stats))
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m Model) renderChatView() string {
	var sb strings.Builder

	hasActiveContent := len(m.chatMessages) > m.committedUpTo || m.streaming

	if hasActiveContent {
		bottomLines := 4
		if m.permDialog {
			bottomLines += 3
		}
		vpH := m.height - bottomLines
		if vpH < 1 {
			vpH = 1
		}
		m.viewport.Height = m.calcViewportHeight(vpH)
		sb.WriteString(m.viewport.View())
		sb.WriteString("\n")
	}

	if m.permDialog {
		sb.WriteString(m.renderPermDialog())
	}
	if m.planApprovalDialog {
		sb.WriteString(m.renderPlanApprovalDialog())
	}
	if m.askUserDialog {
		sb.WriteString(m.renderAskUserDialog())
	}
	if m.rewindDialog {
		sb.WriteString(m.renderRewindDialog())
	}
	if m.sandboxDialog {
		sb.WriteString(m.renderSandboxDialog())
	}
	sb.WriteString(m.renderSeparator())
	sb.WriteString("\n")
	if m.planApprovalDialog {
		// Hide input when plan approval dialog is active
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  Select an option above..."))
	} else {
		sb.WriteString(promptStyle.Render("❯ "))
		sb.WriteString(m.textarea.View())
	}
	sb.WriteString("\n")
	sb.WriteString(m.renderSeparator())
	sb.WriteString("\n")
	if m.slashMenuOpen && len(m.slashMatches) > 0 {
		sb.WriteString(m.renderSlashMenu())
	}
	if m.atMenuOpen && len(m.atMatches) > 0 {
		sb.WriteString(m.renderAtMenu())
	}
	sb.WriteString(m.renderStatusBar())
	return sb.String()
}

func (m Model) renderBanner() string {
	cat := bannerDimStyle.Render("Zebracode v0.1.0") + "\n" +
		bannerDimStyle.Render(m.getModelName()) + "\n" +
		bannerDimStyle.Render(m.getWorkDir())
	return cat
}

func (m Model) renderSeparator() string {
	line := strings.Repeat("─", m.width)
	return separatorStyle.Render(line)
}

func (m Model) renderStatusBar() string {
	left := "  default"
	if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Mode != permissions.ModeDefault {
		name, _ := permissionModeInfo(m.ag.Checker.Mode)
		var modeColor lipgloss.Color
		switch m.ag.Checker.Mode {
		case permissions.ModeAcceptEdits:
			modeColor = greenText
		case permissions.ModePlan:
			modeColor = yellowText
		case permissions.ModeBypass:
			modeColor = redText
		default:
			modeColor = dimText
		}
		modeStr := lipgloss.NewStyle().Foreground(modeColor).Render(
			fmt.Sprintf("%s on", name),
		)
		hint := lipgloss.NewStyle().Foreground(dimText).Render(" (shift+tab to cycle)")
		left = statusBarStyle.Render("  ") + modeStr + hint
	} else {
		left = statusBarStyle.Render(left)
	}

	right := ""
	if m.teamMgr != nil {
		activeCount := 0
		for _, p := range m.teamMgr.GetAllTeammateProgress() {
			if p.GetStatus() == "running" {
				activeCount++
			}
		}
		if activeCount > 0 {
			label := fmt.Sprintf("● %d teammate", activeCount)
			if activeCount > 1 {
				label += "s"
			}
			right += lipgloss.NewStyle().Foreground(cyanText).Render(label + " ")
		}
	}
	if m.mcpConnecting {
		right += lipgloss.NewStyle().Foreground(yellowText).Render("MCP connecting… ")
	}
	if m.selectedProvider != nil {
		right += statusItemStyle.Render(m.selectedProvider.Model)
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 0 {
		gap = 0
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) DumpHistory() string {
	if len(m.chatMessages) == 0 {
		return ""
	}
	var sb strings.Builder

	sb.WriteString(m.renderBanner())
	sb.WriteString("\n\n")

	for _, msg := range m.chatMessages {
		switch msg.role {
		case "user":
			sb.WriteString(promptStyle.Render("❯ "))
			sb.WriteString(lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(msg.content))
			sb.WriteString("\n\n")

		case "assistant":
			sb.WriteString(aiMarkerStyle.Render("● "))
			rendered := m.renderMarkdown(msg.content)
			indented := indentBlock(rendered, "  ")
			sb.WriteString(strings.TrimLeft(indented, " "))
			sb.WriteString("\n\n")

		case "tool", "tool_visible":
			sb.WriteString("  ")
			if strings.HasPrefix(msg.content, "✗") {
				sb.WriteString(toolErrorStyle.Render(msg.content))
			} else {
				sb.WriteString(toolDoneStyle.Render(msg.content))
			}
			sb.WriteString("\n")
			appendEditDiff(&sb, msg.toolGroup)

		case "sub_agent":
			if msg.subAgentBlock != nil {
				sb.WriteString(renderSubAgentBlock(msg.subAgentBlock, false))
			}

		case "system":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")

		case "error":
			sb.WriteString(errorStyle.Render("✖ " + msg.content))
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

func (m Model) renderChatContent() string {
	var sb strings.Builder

	for _, msg := range m.chatMessages[m.committedUpTo:] {
		switch msg.role {
		case "user":
			sb.WriteString(promptStyle.Render("❯ "))
			sb.WriteString(lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(msg.content))
			sb.WriteString("\n\n")

		case "assistant":
			sb.WriteString(aiMarkerStyle.Render("● "))
			rendered := m.renderMarkdown(msg.content)
			indented := indentBlock(rendered, "  ")
			sb.WriteString(strings.TrimLeft(indented, " "))
			sb.WriteString("\n\n")

		case "tool", "tool_visible":
			sb.WriteString("  ")
			if strings.HasPrefix(msg.content, "✗") {
				sb.WriteString(toolErrorStyle.Render(msg.content))
			} else {
				sb.WriteString(toolDoneStyle.Render(msg.content))
			}
			sb.WriteString("\n")
			appendEditDiff(&sb, msg.toolGroup)

		case "tool_collapsed":
			if msg.expanded {
				for _, tb := range msg.toolGroup {
					sb.WriteString("  ")
					text := renderToolBlockText(tb)
					if tb.isError {
						sb.WriteString(toolErrorStyle.Render(text))
					} else {
						sb.WriteString(toolDoneStyle.Render(text))
					}
					sb.WriteString("\n")
				}
			}
			// Hidden when collapsed — no output

		case "tool_group":
			if msg.expanded {
				for _, tb := range msg.toolGroup {
					sb.WriteString("  ")
					text := renderToolBlockText(tb)
					if tb.isError {
						sb.WriteString(toolErrorStyle.Render(text))
					} else {
						sb.WriteString(toolDoneStyle.Render(text))
					}
					sb.WriteString("\n")
				}
			} else {
				summary := renderToolGroupSummary(msg.toolGroup)
				sb.WriteString(toolDoneStyle.Render("  " + summary))
				sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  (ctrl+o to expand)"))
				sb.WriteString("\n")
			}

		case "sub_agent":
			if msg.subAgentBlock != nil {
				sb.WriteString(renderSubAgentBlock(msg.subAgentBlock, msg.expanded))
			}

		case "thinking":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")

		case "system":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")

		case "error":
			sb.WriteString(errorStyle.Render("✖ " + msg.content))
			sb.WriteString("\n\n")
		}
	}

	// Active sub-agent progress (live)
	if m.activeSubAgent != nil && !m.activeSubAgent.done {
		sb.WriteString(renderSubAgentBlock(m.activeSubAgent, false))
	}

	// Active tool blocks
	for _, tb := range m.toolBlocks {
		if tb.toolName == "Agent" && m.activeSubAgent != nil {
			continue // rendered above as sub-agent block
		}
		sb.WriteString(m.renderToolBlock(tb))
		sb.WriteString("\n")
	}

	// Streaming text
	if m.streaming && m.streamBuf != "" {
		sb.WriteString(aiMarkerStyle.Render("● "))
		indented := indentBlock(m.streamBuf, "  ")
		sb.WriteString(streamingTextStyle.Render(strings.TrimLeft(indented, " ")))
		sb.WriteString("\n")
	}

	// Spinner — always last while agent is running
	if m.streaming {
		elapsed := time.Since(m.thinkingStart).Seconds()
		sb.WriteString("\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Render(
			fmt.Sprintf("  %s %s…  (%.0fs)", m.spinner.View(), m.thinkingVerb, elapsed),
		))
		sb.WriteString("\n")
		// Teammate progress tree
		sb.WriteString(m.renderTeammateTree())
	}

	// Show teammate tree even when not streaming (teammates may still be running)
	if !m.streaming {
		tree := m.renderTeammateTree()
		if tree != "" {
			sb.WriteString(tree)
		}
	}

	return sb.String()
}

func (m Model) renderMessagesRange(from, to int) string {
	var sb strings.Builder
	for i := from; i < to && i < len(m.chatMessages); i++ {
		msg := m.chatMessages[i]
		switch msg.role {
		case "user":
			sb.WriteString(promptStyle.Render("❯ "))
			sb.WriteString(lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(msg.content))
			sb.WriteString("\n\n")
		case "assistant":
			sb.WriteString(aiMarkerStyle.Render("● "))
			rendered := m.renderMarkdown(msg.content)
			indented := indentBlock(rendered, "    ")
			sb.WriteString(strings.TrimLeft(indented, " "))
			sb.WriteString("\n\n")
		case "tool", "tool_visible":
			sb.WriteString("  ")
			if strings.HasPrefix(msg.content, "✗") {
				sb.WriteString(toolErrorStyle.Render(msg.content))
			} else {
				sb.WriteString(toolDoneStyle.Render(msg.content))
			}
			sb.WriteString("\n")
			appendEditDiff(&sb, msg.toolGroup)
		case "tool_collapsed":
			// Scrollback can't be re-expanded later, so always render each
			// tool inline (with name + args) instead of collapsing the group.
			for _, tb := range msg.toolGroup {
				sb.WriteString("  ")
				text := renderToolBlockText(tb)
				if tb.isError {
					sb.WriteString(toolErrorStyle.Render(text))
				} else {
					sb.WriteString(toolDoneStyle.Render(text))
				}
				sb.WriteString("\n")
			}
		case "tool_group":
			summary := renderToolGroupSummary(msg.toolGroup)
			sb.WriteString(toolDoneStyle.Render("  " + summary))
			sb.WriteString("\n")
		case "sub_agent":
			if msg.subAgentBlock != nil {
				sb.WriteString(renderSubAgentBlock(msg.subAgentBlock, false))
			}
		case "thinking":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")
		case "system":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")
		case "error":
			sb.WriteString(errorStyle.Render("✖ " + msg.content))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}
