package tui

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/ericthz/zebra-code/internal/agent"
	"github.com/ericthz/zebra-code/internal/commands"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/planfile"
	"github.com/ericthz/zebra-code/internal/prompt"
	"github.com/ericthz/zebra-code/internal/sandbox"
	"github.com/ericthz/zebra-code/internal/tools"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var permOptions = []struct {
	label    string
	response agent.PermissionResponse
}{
	{"Yes", agent.PermAllow},
	{"Yes, and don't ask again for this pattern", agent.PermAllowAlways},
	{"No", agent.PermDeny},
}

func (m Model) handlePermDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.permCursor > 0 {
			m.permCursor--
		}
		return m, nil
	case "down":
		if m.permCursor < len(permOptions)-1 {
			m.permCursor++
		}
		return m, nil
	case "enter":
		m.permRespCh <- permOptions[m.permCursor].response
		m.permDialog = false
		m.permCursor = 0
		m.updateViewport()
		return m, m.listenForAgentEvents()
	case "escape":
		m.permRespCh <- agent.PermDeny
		m.permDialog = false
		m.permCursor = 0
		m.updateViewport()
		return m, m.listenForAgentEvents()
	}
	return m, nil
}

func (m Model) handlePlanApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.planApprovalCursor > 0 {
			m.planApprovalCursor--
		}
		m.updateViewport()
		return m, nil
	case "down", "j":
		if m.planApprovalCursor < 2 {
			m.planApprovalCursor++
		}
		m.updateViewport()
		return m, nil
	case "enter":
		if m.planApprovalCursor == 2 && m.planApprovalInput != "" {
			return m.sendPlanFeedback(m.planApprovalInput, false)
		}
		return m.executePlanApproval()
	case "shift+tab":
		if m.planApprovalCursor == 2 && m.planApprovalInput != "" {
			return m.sendPlanFeedback(m.planApprovalInput, true)
		}
		return m, nil
	case "escape":
		m.planApprovalDialog = false
		m.updateViewport()
		return m, nil
	case "backspace":
		if m.planApprovalCursor == 2 && len(m.planApprovalInput) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.planApprovalInput)
			m.planApprovalInput = m.planApprovalInput[:len(m.planApprovalInput)-size]
			m.updateViewport()
		}
		return m, nil
	default:
		if m.planApprovalCursor == 2 && len(msg.Runes) > 0 {
			m.planApprovalInput += string(msg.Runes)
			m.updateViewport()
		}
		return m, nil
	}
}

func (m Model) executePlanApproval() (tea.Model, tea.Cmd) {
	m.planApprovalDialog = false
	wd, _ := os.Getwd()

	var modeMsg string
	switch m.planApprovalCursor {
	case 0: // YOLO mode
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.Mode = permissions.ModeBypass
			m.ag.Checker.PlanFilePath = ""
		}
		modeMsg = "Plan approved. Entered YOLO mode (all operations auto-approved)."
	case 1: // Manually approve
		if m.ag != nil && m.ag.Checker != nil {
			restoreMode := m.prePlanMode
			if restoreMode == "" {
				restoreMode = permissions.ModeDefault
			}
			m.ag.Checker.Mode = restoreMode
			m.ag.Checker.PlanFilePath = ""
		}
		modeMsg = "Plan approved. Each edit will require your confirmation."
	}

	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: modeMsg,
	})

	// Load the plan and send it as context for the agent to start executing
	planPath := planfile.GetPlanFilePath(wd)
	planContent, _ := planfile.LoadPlan(wd)
	planExists := planfile.PlanExists(wd)
	planfile.ResetPlanPath()

	executeMsg := prompt.BuildPlanModeExitReminder(planPath, planExists)
	// 标记本次会话已退出过 Plan Mode，后续重入时可注入提示
	m.hasExitedPlanMode = true
	executeMsg += "\n\nUser has approved your plan. You can now start coding."
	if planContent != "" {
		executeMsg += "\n\nApproved Plan:\n" + planContent
	}

	m.updateViewport()
	return m.sendMessage(executeMsg)
}

func (m Model) sendPlanFeedback(feedback string, alsoExit bool) (tea.Model, tea.Cmd) {
	m.planApprovalDialog = false
	m.planApprovalInput = ""

	if alsoExit {
		if m.ag != nil && m.ag.Checker != nil {
			restoreMode := m.prePlanMode
			if restoreMode == "" {
				restoreMode = permissions.ModeDefault
			}
			m.ag.Checker.Mode = restoreMode
			m.ag.Checker.PlanFilePath = ""
		}
		planfile.ResetPlanPath()
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: "Exiting plan mode with feedback. Edits will require confirmation.",
		})
	}

	m.updateViewport()
	return m.sendMessage(feedback)
}

func (m Model) renderPlanApprovalDialog() string {
	var sb strings.Builder

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(
		" Zebracode has written up a plan and is ready to execute. Would you like to proceed?",
	)
	sb.WriteString(header)
	sb.WriteString("\n\n")

	options := []string{
		"Yes, enter YOLO mode (auto-approve all)",
		"Yes, manually approve edits",
		"Tell Zebracode what to change",
	}

	for i, opt := range options {
		prefix := "   "
		if i == m.planApprovalCursor {
			prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
		}
		label := opt
		if i == m.planApprovalCursor {
			label = lipgloss.NewStyle().Bold(true).Render(opt)
		} else {
			label = lipgloss.NewStyle().Foreground(dimText).Render(opt)
		}
		sb.WriteString(prefix)
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, label))
		sb.WriteString("\n")

		if i == 2 {
			inputLine := m.planApprovalInput
			if m.planApprovalCursor == 2 {
				inputLine += "█"
			}
			if inputLine == "█" || inputLine == "" {
				placeholder := lipgloss.NewStyle().Foreground(dimText).Render("Type feedback here...")
				if m.planApprovalCursor == 2 {
					sb.WriteString("      " + placeholder + "\n")
				}
			} else {
				sb.WriteString("      " + inputLine + "\n")
			}
			hint := lipgloss.NewStyle().Foreground(dimText).Render("      shift+tab to approve with this feedback")
			sb.WriteString(hint)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// handleSandboxDialog 处理沙箱模式选择对话框的按键交互

func (m Model) handleSandboxDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	labels := commands.SandboxModeLabels()
	switch msg.String() {
	case "up", "k":
		if m.sandboxCursor > 0 {
			m.sandboxCursor--
		}
		m.updateViewport()
		return m, nil
	case "down", "j":
		if m.sandboxCursor < len(labels)-1 {
			m.sandboxCursor++
		}
		m.updateViewport()
		return m, nil
	case "enter":
		m.sandboxDialog = false
		mode := commands.SandboxMode(m.sandboxCursor)
		return m.applySandboxMode(mode)
	case "escape":
		m.sandboxDialog = false
		m.updateViewport()
		return m, nil
	}
	return m, nil
}

// applySandboxMode 根据选择的模式更新 BashTool 和权限检查器

func (m Model) applySandboxMode(mode commands.SandboxMode) (tea.Model, tea.Cmd) {
	bashTool, _ := m.registry.Get("Bash").(*tools.BashTool)
	labels := commands.SandboxModeLabels()
	descriptions := commands.SandboxModeDescriptions()

	switch mode {
	case commands.SandboxAutoAllow:
		// 启用沙箱 + 自动放行
		sb := sandbox.New()
		if bashTool != nil {
			bashTool.Sandbox = sb
			if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Sandbox != nil {
				bashTool.SandboxConfig = sandbox.Config{
					AllowWrite:     m.ag.Checker.Sandbox.GetAllowedRoots(),
					DenyWrite:      m.ag.Checker.Sandbox.GetDenyWrite(),
					NetworkEnabled: false,
				}
			}
		}
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.SandboxEnabled = true
		}
	case commands.SandboxRegular:
		// 启用沙箱但保留常规权限确认
		sb := sandbox.New()
		if bashTool != nil {
			bashTool.Sandbox = sb
			if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Sandbox != nil {
				bashTool.SandboxConfig = sandbox.Config{
					AllowWrite:     m.ag.Checker.Sandbox.GetAllowedRoots(),
					DenyWrite:      m.ag.Checker.Sandbox.GetDenyWrite(),
					NetworkEnabled: false,
				}
			}
		}
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.SandboxEnabled = false
		}
	case commands.SandboxOff:
		// 关闭沙箱
		if bashTool != nil {
			bashTool.Sandbox = nil
			bashTool.SandboxConfig = sandbox.Config{}
		}
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.SandboxEnabled = false
		}
	}

	msg := fmt.Sprintf("沙箱模式已切换：%s\n%s", labels[mode], descriptions[mode])
	m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: msg})
	m.updateViewport()
	return m, nil
}

// renderSandboxDialog 渲染沙箱模式选择界面

func (m Model) renderSandboxDialog() string {
	if !m.sandboxDialog {
		return ""
	}
	var sb strings.Builder

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(
		" 选择沙箱模式",
	)
	sb.WriteString(header)
	sb.WriteString("\n\n")

	labels := commands.SandboxModeLabels()
	descs := commands.SandboxModeDescriptions()

	for i, label := range labels {
		prefix := "   "
		if i == m.sandboxCursor {
			prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
		}
		displayLabel := label
		if i == m.sandboxCursor {
			displayLabel = lipgloss.NewStyle().Bold(true).Render(label)
		} else {
			displayLabel = lipgloss.NewStyle().Foreground(dimText).Render(label)
		}
		desc := lipgloss.NewStyle().Foreground(dimText).Render(" — " + descs[i])
		sb.WriteString(fmt.Sprintf("%s%d. %s%s\n", prefix, i+1, displayLabel, desc))
	}
	sb.WriteString("\n")
	return sb.String()
}

func (m Model) handleAskUserDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	multiQuestion := len(m.askUserQuestions) > 1

	// Submit tab handling
	if m.askUserOnSubmit {
		switch msg.String() {
		case "up", "k":
			if m.askUserSubmitIdx > 0 {
				m.askUserSubmitIdx--
			}
			m.updateViewport()
			return m, nil
		case "down", "j":
			if m.askUserSubmitIdx < 1 {
				m.askUserSubmitIdx++
			}
			m.updateViewport()
			return m, nil
		case "left", "shift+tab":
			m.askUserOnSubmit = false
			m.askUserQIdx = len(m.askUserQuestions) - 1
			m.updateViewport()
			return m, nil
		case "enter":
			if m.askUserSubmitIdx == 0 {
				return m.submitAllAnswers()
			}
			return m.cancelAskUser()
		case "escape":
			return m.cancelAskUser()
		}
		return m, nil
	}

	q := m.askUserQuestions[m.askUserQIdx]
	optCount := len(q.Options) + 1
	cursor := m.askUserCursors[m.askUserQIdx]

	switch msg.String() {
	case "up", "k":
		if cursor > 0 {
			m.askUserCursors[m.askUserQIdx]--
		}
		m.updateViewport()
		return m, nil
	case "down", "j":
		if cursor < optCount-1 {
			m.askUserCursors[m.askUserQIdx]++
		}
		m.updateViewport()
		return m, nil
	case "left", "shift+tab":
		if multiQuestion && m.askUserQIdx > 0 {
			m.askUserQIdx--
			m.updateViewport()
		}
		return m, nil
	case "right", "tab":
		if multiQuestion {
			if m.askUserQIdx < len(m.askUserQuestions)-1 {
				m.askUserQIdx++
			} else {
				m.askUserOnSubmit = true
				m.askUserSubmitIdx = 0
			}
			m.updateViewport()
		}
		return m, nil
	case " ":
		if q.MultiSelect && cursor < len(q.Options) {
			sel := m.askUserSelected[m.askUserQIdx]
			sel[cursor] = !sel[cursor]
			m.updateViewport()
			return m, nil
		}
	case "enter":
		m.saveCurrentAnswer()

		if !multiQuestion && !q.MultiSelect {
			return m.submitAllAnswers()
		}

		if m.askUserQIdx < len(m.askUserQuestions)-1 {
			m.askUserQIdx++
		} else {
			m.askUserOnSubmit = true
			m.askUserSubmitIdx = 0
		}
		m.updateViewport()
		return m, nil
	case "backspace":
		if cursor == len(q.Options) && len(m.askUserOther[m.askUserQIdx]) > 0 {
			s := m.askUserOther[m.askUserQIdx]
			_, size := utf8.DecodeLastRuneInString(s)
			m.askUserOther[m.askUserQIdx] = s[:len(s)-size]
			m.updateViewport()
		}
		return m, nil
	case "escape":
		return m.cancelAskUser()
	default:
		if cursor == len(q.Options) && len(msg.Runes) > 0 {
			m.askUserOther[m.askUserQIdx] += string(msg.Runes)
			m.updateViewport()
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) saveCurrentAnswer() {
	q := m.askUserQuestions[m.askUserQIdx]
	cursor := m.askUserCursors[m.askUserQIdx]

	if cursor == len(q.Options) {
		other := m.askUserOther[m.askUserQIdx]
		if other == "" {
			other = "Other"
		}
		m.askUserAnswered[m.askUserQIdx] = other
	} else if q.MultiSelect {
		var selected []string
		for i, opt := range q.Options {
			if m.askUserSelected[m.askUserQIdx][i] {
				selected = append(selected, opt.Label)
			}
		}
		if len(selected) == 0 {
			selected = append(selected, q.Options[cursor].Label)
		}
		m.askUserAnswered[m.askUserQIdx] = strings.Join(selected, ", ")
	} else {
		m.askUserAnswered[m.askUserQIdx] = q.Options[cursor].Label
	}
}

func (m *Model) collectAskUserAnswers() map[string]string {
	result := make(map[string]string)
	for idx, answer := range m.askUserAnswered {
		if idx < len(m.askUserQuestions) {
			result[m.askUserQuestions[idx].Text] = answer
		}
	}
	return result
}

func (m *Model) submitAllAnswers() (tea.Model, tea.Cmd) {
	m.askUserDialog = false
	m.askUserRespCh <- tools.QuestionResponse{Answers: m.collectAskUserAnswers()}
	m.updateViewport()
	return m, tea.Batch(m.listenForAgentEvents(), m.listenForAskUser())
}

func (m *Model) cancelAskUser() (tea.Model, tea.Cmd) {
	m.askUserDialog = false
	m.askUserRespCh <- tools.QuestionResponse{Answers: map[string]string{"_declined": "true"}}
	m.updateViewport()
	return m, tea.Batch(m.listenForAgentEvents(), m.listenForAskUser())
}

func (m Model) renderAskUserDialog() string {
	if !m.askUserDialog || len(m.askUserQuestions) == 0 {
		return ""
	}
	var sb strings.Builder
	multiQuestion := len(m.askUserQuestions) > 1

	// Navigation bar (only for multi-question)
	if multiQuestion {
		sb.WriteString(m.renderQuestionNavBar())
		sb.WriteString("\n\n")
	}

	if m.askUserOnSubmit {
		sb.WriteString(m.renderSubmitView())
	} else {
		sb.WriteString(m.renderQuestionView())
	}

	// Bottom hint
	if multiQuestion && !m.askUserOnSubmit {
		hint := lipgloss.NewStyle().Foreground(dimText).Render("      ← → navigate questions · enter to confirm")
		sb.WriteString(hint)
		sb.WriteString("\n\n")
	}

	return sb.String()
}

func (m Model) renderQuestionNavBar() string {
	var sb strings.Builder
	activeTab := lipgloss.NewStyle().Background(lipgloss.Color("99")).Foreground(lipgloss.Color("255")).Bold(true).Padding(0, 1)
	inactiveTab := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Padding(0, 1)
	dimArrow := lipgloss.NewStyle().Foreground(dimText)
	brightArrow := lipgloss.NewStyle().Foreground(brandPurple).Bold(true)

	// Left arrow
	if m.askUserQIdx == 0 && !m.askUserOnSubmit {
		sb.WriteString(dimArrow.Render(" ←"))
	} else {
		sb.WriteString(brightArrow.Render(" ←"))
	}

	// Question tabs
	for i, q := range m.askUserQuestions {
		header := q.Header
		if header == "" {
			header = fmt.Sprintf("Q%d", i+1)
		}
		_, answered := m.askUserAnswered[i]
		check := "☐"
		if answered {
			check = "☑"
		}
		label := header + " " + check

		if !m.askUserOnSubmit && i == m.askUserQIdx {
			sb.WriteString(activeTab.Render(label))
		} else {
			sb.WriteString(inactiveTab.Render(label))
		}
	}

	// Submit tab
	submitLabel := "✓ Submit"
	if m.askUserOnSubmit {
		sb.WriteString(activeTab.Render(submitLabel))
	} else {
		sb.WriteString(inactiveTab.Render(submitLabel))
	}

	// Right arrow
	if m.askUserOnSubmit {
		sb.WriteString(dimArrow.Render(" →"))
	} else {
		sb.WriteString(brightArrow.Render(" →"))
	}

	return sb.String()
}

func (m Model) askUserMaxLines() int {
	maxLines := 0
	for _, q := range m.askUserQuestions {
		lines := 2 + len(q.Options) + 1 // header + blank + options + Other
		if q.MultiSelect {
			lines++ // "space to toggle" hint
		}
		if lines > maxLines {
			maxLines = lines
		}
	}
	return maxLines
}

func (m Model) renderQuestionView() string {
	var sb strings.Builder
	q := m.askUserQuestions[m.askUserQIdx]
	cursor := m.askUserCursors[m.askUserQIdx]
	lines := 0

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(" " + q.Text)
	sb.WriteString(header)
	sb.WriteString("\n\n")
	lines += 2

	for i, opt := range q.Options {
		prefix := "   "
		if i == cursor {
			prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
		}
		if q.MultiSelect {
			check := "○"
			if m.askUserSelected[m.askUserQIdx][i] {
				check = "●"
			}
			prefix += check + " "
		}
		label := opt.Label
		if i == cursor {
			label = lipgloss.NewStyle().Bold(true).Render(opt.Label)
		}
		desc := lipgloss.NewStyle().Foreground(dimText).Render(" — " + opt.Description)
		sb.WriteString(fmt.Sprintf("%s%s%s\n", prefix, label, desc))
		lines++
	}

	// "Other" option
	otherIdx := len(q.Options)
	prefix := "   "
	if cursor == otherIdx {
		prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
	}
	otherLabel := "Other"
	if cursor == otherIdx {
		otherLabel = lipgloss.NewStyle().Bold(true).Render("Other")
	} else {
		otherLabel = lipgloss.NewStyle().Foreground(dimText).Render("Other")
	}
	sb.WriteString(fmt.Sprintf("%s%s", prefix, otherLabel))
	if cursor == otherIdx {
		input := m.askUserOther[m.askUserQIdx] + "█"
		sb.WriteString(": " + input)
	}
	sb.WriteString("\n")
	lines++

	if q.MultiSelect {
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("      space to toggle, enter to confirm"))
		sb.WriteString("\n")
		lines++
	}

	// Pad to fixed height so switching questions doesn't cause layout shift
	if len(m.askUserQuestions) > 1 {
		target := m.askUserMaxLines()
		for lines < target {
			sb.WriteString("\n")
			lines++
		}
	}

	return sb.String()
}

func (m Model) renderSubmitView() string {
	var sb strings.Builder
	lines := 0

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(" Review your answers:")
	sb.WriteString(header)
	sb.WriteString("\n\n")
	lines += 2

	for i, q := range m.askUserQuestions {
		label := q.Header
		if label == "" {
			label = fmt.Sprintf("Q%d", i+1)
		}
		answer, ok := m.askUserAnswered[i]
		if ok {
			sb.WriteString(fmt.Sprintf("   %s: %s\n", label, answer))
		} else {
			dim := lipgloss.NewStyle().Foreground(dimText).Render(fmt.Sprintf("   %s: (not answered)", label))
			sb.WriteString(dim + "\n")
		}
		lines++
	}
	sb.WriteString("\n")
	lines++

	// Submit / Cancel options
	for i, opt := range []string{"Submit answers", "Cancel"} {
		if i == m.askUserSubmitIdx {
			prefix := lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
			label := lipgloss.NewStyle().Bold(true).Render(opt)
			sb.WriteString(prefix + label + "\n")
		} else {
			sb.WriteString("   " + opt + "\n")
		}
		lines++
	}

	// Pad to match question view height
	target := m.askUserMaxLines()
	for lines < target {
		sb.WriteString("\n")
		lines++
	}

	return sb.String()
}
