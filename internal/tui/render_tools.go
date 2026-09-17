package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func renderToolBlockText(tb toolBlockInfo) string {
	title := toolTitle(tb.toolName, tb.args)
	if tb.isError {
		return fmt.Sprintf("✗ %s (%.1fs)", title, tb.elapsed)
	}
	return fmt.Sprintf("✓ %s (%.1fs)", title, tb.elapsed)
}

func renderSubAgentBlock(sab *subAgentBlock, expanded bool) string {
	var sb strings.Builder

	agentLabel := strings.Title(sab.agentType)
	if agentLabel == "" {
		agentLabel = "Agent"
	}
	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(
		fmt.Sprintf("● %s(%s)", agentLabel, sab.desc))
	sb.WriteString(header)
	sb.WriteString("\n")

	if sab.done {
		if expanded {
			for _, tu := range sab.toolUses {
				title := toolTitle(tu.toolName, tu.args)
				line := fmt.Sprintf("     %s (%.1fs)", title, tu.elapsed)
				if tu.isError {
					sb.WriteString(toolErrorStyle.Render(line))
				} else {
					sb.WriteString(toolDoneStyle.Render(line))
				}
				sb.WriteString("\n")
			}
		} else {
			summary := fmt.Sprintf("  ⎿  Done (%d tool uses · %.1fs)", sab.toolCount, sab.totalTime)
			sb.WriteString(toolDoneStyle.Render(summary))
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  (ctrl+o to expand)"))
			sb.WriteString("\n")
		}
	} else {
		n := len(sab.toolUses)
		if n > 0 {
			last := sab.toolUses[n-1]
			lastTitle := toolTitle(last.toolName, last.args)
			sb.WriteString(toolDoneStyle.Render(fmt.Sprintf("  ⎿  %s (%.1fs)", lastTitle, last.elapsed)))
			sb.WriteString("\n")
		}
		if n > 1 {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render(
				fmt.Sprintf("     … +%d tool uses (ctrl+o to expand)", n-1)))
			sb.WriteString("\n")
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("     Running…"))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func isCollapsibleTool(name string) bool {
	switch name {
	case "ReadFile", "Glob", "Grep", "ToolSearch":
		return true
	}
	return false
}

// isDiffTool 判断该工具的 output 是不是 BuildDiff 生成的带行号 diff 文本。

func isDiffTool(name string) bool {
	return name == "EditFile"
}

// renderDiffLines 把 tools.BuildDiff() 产出的带行号 diff 文本渲染成彩色行：
// "+ " 开头绿色、"- " 开头红色，其余（上下文行/摘要行）走 toolDetailStyle。

func renderDiffLines(output string) string {
	lines := strings.Split(output, "\n")
	rendered := make([]string, len(lines))
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "+ "):
			rendered[i] = diffAddStyle.Render(line)
		case strings.HasPrefix(line, "- "):
			rendered[i] = diffRemoveStyle.Render(line)
		default:
			rendered[i] = toolDetailStyle.Render(line)
		}
	}
	return strings.Join(rendered, "\n")
}

// appendEditDiff 在已渲染好的工具标题行后面追加 EditFile 的 diff 正文（如果有）。

func appendEditDiff(sb *strings.Builder, toolGroup []toolBlockInfo) {
	if len(toolGroup) != 1 {
		return
	}
	tb := toolGroup[0]
	if !isDiffTool(tb.toolName) || tb.output == "" {
		return
	}
	sb.WriteString(renderDiffLines(tb.output))
	sb.WriteString("\n")
}

func renderToolGroupSummary(tools []toolBlockInfo) string {
	var totalElapsed float64
	errors := 0
	for _, tb := range tools {
		totalElapsed += tb.elapsed
		if tb.isError {
			errors++
		}
	}
	n := len(tools)
	if errors > 0 {
		return fmt.Sprintf("● Done (%d tool uses · %d errors · %.1fs)", n, errors, totalElapsed)
	}
	return fmt.Sprintf("● Done (%d tool uses · %.1fs)", n, totalElapsed)
}

func (m *Model) calcViewportHeight(availableHeight int) int {
	contentLines := m.viewport.TotalLineCount()
	if contentLines < 1 {
		contentLines = 1
	}
	if contentLines > availableHeight {
		return availableHeight
	}
	return contentLines
}

func (m *Model) updateViewport() {
	if !m.ready {
		return
	}
	content := m.renderChatContent()
	m.viewport.SetContent(content)

	statusHeight, sepHeight := 1, 1
	inputHeight := m.textarea.Height() + 1
	available := m.height - statusHeight - sepHeight - inputHeight - 1
	if available < 1 {
		available = 1
	}
	m.viewport.Height = m.calcViewportHeight(available)

	if !m.userScrolled {
		m.viewport.GotoBottom()
	}
}

// ── View ──

func (m Model) renderToolBlock(tb toolBlockInfo) string {
	title := toolTitle(tb.toolName, tb.args)

	if tb.loading {
		return toolRunningStyle.Render(fmt.Sprintf("● %s …", title))
	}

	if tb.isError {
		return toolErrorStyle.Render(fmt.Sprintf("✗ %s — error (%.1fs)", title, tb.elapsed))
	}

	return toolDoneStyle.Render(fmt.Sprintf("✓ %s (%.1fs)", title, tb.elapsed))
}
