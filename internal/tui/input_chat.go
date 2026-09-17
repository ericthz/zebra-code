package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ericthz/zebra-code/internal/agent"
	"github.com/ericthz/zebra-code/internal/agents"
	"github.com/ericthz/zebra-code/internal/commands"
	"github.com/ericthz/zebra-code/internal/filehistory"
	"github.com/ericthz/zebra-code/internal/history"
	"github.com/ericthz/zebra-code/internal/hooks"
	"github.com/ericthz/zebra-code/internal/llm"
	"github.com/ericthz/zebra-code/internal/memory"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/sandbox"
	"github.com/ericthz/zebra-code/internal/session"
	"github.com/ericthz/zebra-code/internal/teams"
	"github.com/ericthz/zebra-code/internal/tools"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rivo/uniseg"
)

func (m Model) handleProviderSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.providerCursor > 0 {
			m.providerCursor--
		}
	case "down", "j":
		if m.providerCursor < len(m.providers)-1 {
			m.providerCursor++
		}
	case "enter":
		p := &m.providers[m.providerCursor]
		m.selectedProvider = p
		wd, _ := os.Getwd()
		systemPrompt := m.loadSkillsAndBuildPrompt(wd)
		client, err := llm.NewClient(p, systemPrompt)
		if err != nil {
			m.state = stateChat
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: err.Error()})
			return m, nil
		}
		m.client = client
		m.sessionID = session.NewID()
		m.fileHistory = filehistory.New(wd, m.sessionID)
		m.defaultTools.EditFile.FileHistory = m.fileHistory
		m.defaultTools.WriteFile.FileHistory = m.fileHistory
		m.registerAgentTools(client, p, p.Protocol, wd)
		// Best-effort: pull the model's context window from the provider once
		// (Anthropic only) and cache it on p before GetContextWindow reads it.
		// Silently degrades to the mapping table / default on any failure.
		llm.ResolveContextWindow(context.Background(), p)
		ag := agent.New(client, m.registry, p.Protocol)
		ag.ContextWindow = p.GetContextWindow()
		ag.MaxOutputTokens = p.GetMaxOutputTokens()
		ag.Instructions = m.instructionsContent
		ag.MemoryContent = m.memoryContent
		ag.FileHistory = m.fileHistory
		ag.SetSessionID(m.sessionID)
		sandboxAllow := []string{memory.GetAutoMemPath(wd)}
		if userMem := memory.GetUserAutoMemPath(); userMem != "" {
			sandboxAllow = append(sandboxAllow, userMem)
		}
		pathSandbox2 := permissions.NewPathSandbox(wd, sandboxAllow...)
		ag.Checker = permissions.NewChecker(
			pathSandbox2,
			&permissions.RuleEngine{
				LocalPath: filepath.Join(wd, ".zebracode", "permissions.local.yaml"),
			},
			permissions.ModeDefault,
		)
		// 根据配置文件初始化 OS 级沙箱
		if m.sandboxCfg.Enabled {
			sb := sandbox.New()
			if bashTool, ok := m.registry.Get("Bash").(*tools.BashTool); ok && sb != nil {
				bashTool.Sandbox = sb
				bashTool.SandboxConfig = sandbox.Config{
					AllowWrite:     pathSandbox2.GetAllowedRoots(),
					DenyWrite:      pathSandbox2.GetDenyWrite(),
					NetworkEnabled: m.sandboxCfg.NetworkEnabled,
				}
			}
			if m.sandboxCfg.AutoAllow {
				ag.Checker.SandboxEnabled = true
			}
		}
		ag.NotificationFn = m.drainTaskNotifications
		ag.ToolNameFilter = teams.CoordinatorToolFilter(m.EnableCoordinatorMode)
		ag.CoordinatorActiveFn = teams.CoordinatorActiveFn(m.EnableCoordinatorMode)
		if len(m.hookConfigs) > 0 {
			eng := hooks.NewEngine()
			eng.LoadHooks(m.hookConfigs)
			eng.AgentRunner = newAgentHookRunner(client)
			ag.Hooks = eng
		}
		m.ag = ag
		if at, ok := m.registry.Get("Agent").(*agents.AgentTool); ok {
			at.ParentChecker = ag.Checker
		}
		m.wireSkillsToAgent()
		m.memoryExtractor = m.installMemoryExtractor(ag, wd, p.Protocol)
		m.historyEntries = history.Load(wd)
		// NOTE: keep m.sessionID == the id wired into ag (SetSessionID) and into
		// m.fileHistory above; do NOT mint a fresh id here, or compact boundaries
		// would land in a different session file than the one the TUI appends to.
		m.state = stateChat
		m.textarea.Focus()
		m.updateViewport()
		return m, m.initMCPServersCmd()
	}
	return m, nil
}

func (m Model) handleChat(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.askUserDialog {
		return m.handleAskUserDialog(msg)
	}

	if m.planApprovalDialog {
		return m.handlePlanApproval(msg)
	}

	if m.rewindDialog {
		return m.handleRewindKeys(msg)
	}

	if m.sandboxDialog {
		return m.handleSandboxDialog(msg)
	}

	// ctrl+o: toggle expand/collapse on ALL collapsible blocks
	if msg.String() == "ctrl+o" {
		toggled := false
		for i := range m.chatMessages {
			r := m.chatMessages[i].role
			if r == "tool_group" || r == "tool_collapsed" || r == "sub_agent" {
				m.chatMessages[i].expanded = !m.chatMessages[i].expanded
				toggled = true
			}
		}
		if toggled {
			m.updateViewport()
		}
		return m, nil
	}

	// ESC during streaming: adopt running sub-agent to background
	if msg.String() == "escape" && m.streaming && m.agentCh != nil && m.cancelStream != nil {
		if m.taskMgr != nil {
			taskID := m.taskMgr.AdoptRunning("manual-background", m.agentCh, m.cancelStream)
			m.chatMessages = append(m.chatMessages, chatMessage{
				role:    "system",
				content: fmt.Sprintf("Agent moved to background (task %s). You will be notified when it completes.", taskID),
			})
			m.agentCh = nil
			m.cancelStream = nil
			m.finishStreaming()
			commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
			m.committedUpTo = len(m.chatMessages)
			if commitText != "" {
				return m, tea.Println(commitText)
			}
		}
		return m, nil
	}

	if m.atMenuOpen {
		switch msg.String() {
		case "up":
			if m.atCursor > 0 {
				m.atCursor--
			}
			return m, nil
		case "down":
			if m.atCursor < len(m.atMatches)-1 {
				m.atCursor++
			}
			return m, nil
		case "enter", "tab":
			if m.atCursor < len(m.atMatches) {
				selected := m.atMatches[m.atCursor]
				// Replace @prefix with @filepath
				text := m.textarea.Value()
				atIdx := strings.LastIndex(text, "@")
				if atIdx >= 0 {
					m.textarea.Reset()
					m.textarea.SetHeight(1)
					m.textarea.InsertString(text[:atIdx] + "@" + selected + " ")
				}
				m.atMenuOpen = false
				m.atMatches = nil
				m.atCursor = 0
			}
			return m, nil
		case "escape":
			m.atMenuOpen = false
			m.atMatches = nil
			m.atCursor = 0
			return m, nil
		}
	}

	if m.slashMenuOpen {
		switch msg.String() {
		case "up":
			if m.slashCursor > 0 {
				m.slashCursor--
			}
			return m, nil
		case "down":
			if m.slashCursor < len(m.slashMatches)-1 {
				m.slashCursor++
			}
			return m, nil
		case "enter":
			if m.slashCursor < len(m.slashMatches) {
				selected := m.slashMatches[m.slashCursor]
				m.slashMenuOpen = false
				m.slashMatches = nil
				m.slashCursor = 0
				m.textarea.Reset()
				m.textarea.SetHeight(1)
				return m.executeCommand(selected.Name, "")
			}
			return m, nil
		case "escape":
			m.slashMenuOpen = false
			m.slashMatches = nil
			m.slashCursor = 0
			return m, nil
		case "tab":
			if m.slashCursor < len(m.slashMatches) {
				selected := m.slashMatches[m.slashCursor]
				m.textarea.Reset()
				m.textarea.SetHeight(1)
				m.textarea.InsertString("/" + selected.Name + " ")
				m.slashMenuOpen = false
				m.slashMatches = nil
				m.slashCursor = 0
			}
			return m, nil
		}
	}

	if msg.String() == "shift+tab" {
		if m.ag != nil && m.ag.Checker != nil && !m.streaming {
			m.ag.Checker.Mode = nextPermissionMode(m.ag.Checker.Mode)
			m.updateViewport()
		}
		return m, nil
	}

	if msg.String() == "ctrl+j" {
		m.textarea.InsertString("\n")
		m.resizeTextarea()
		return m, nil
	}

	if msg.String() == "enter" {
		text := strings.TrimSpace(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		if m.streaming {
			if m.cancelStream != nil {
				m.cancelStream()
			}
			m.savePartialResponse()
			m.finishStreaming()
		}
		if strings.HasPrefix(text, "/") {
			name, args := commands.Parse(text)
			m.textarea.Reset()
			m.textarea.SetHeight(1)
			m.slashMenuOpen = false
			m.slashMatches = nil
			m.slashCursor = 0
			return m.executeCommand(name, args)
		}
		m.slashMenuOpen = false
		return m.sendMessage(text)
	}

	switch msg.String() {
	case "pgup", "pgdown", "home", "end":
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		m.userScrolled = !m.viewport.AtBottom()
		return m, vpCmd
	case "up":
		if !m.streaming && m.textarea.Line() == 0 {
			m.historyUp()
			return m, nil
		}
	case "down":
		if !m.streaming && m.textarea.Line() == m.textarea.LineCount()-1 {
			m.historyDown()
			return m, nil
		}
	}

	prevText := m.textarea.Value()
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	if m.textarea.Value() != prevText {
		m.historyIndex = 0
		m.resizeTextarea()
	}

	m.updateSlashMenu()
	m.updateAtMenu()

	return m, cmd
}

func (m *Model) resizeTextarea() {
	content := m.textarea.Value()
	textWidth := m.textarea.Width()
	if textWidth < 1 {
		textWidth = 1
	}
	total := 0
	for _, line := range strings.Split(content, "\n") {
		w := uniseg.StringWidth(line)
		if w <= textWidth {
			total++
		} else {
			total += (w + textWidth - 1) / textWidth
		}
	}
	maxH := m.height / 2
	if maxH < 1 {
		maxH = 1
	}
	if total > maxH {
		total = maxH
	}
	if total < 1 {
		total = 1
	}
	m.textarea.SetHeight(total)
	m.updateViewport()
}

func (m *Model) updateSlashMenu() {
	text := m.textarea.Value()
	if !strings.HasPrefix(text, "/") || m.historyIndex > 0 {
		m.slashMenuOpen = false
		m.slashMatches = nil
		m.slashCursor = 0
		return
	}

	prefix := strings.TrimPrefix(text, "/")
	if strings.Contains(prefix, " ") {
		m.slashMenuOpen = false
		m.slashMatches = nil
		m.slashCursor = 0
		return
	}

	names := m.cmdRegistry.Complete(prefix)
	var matches []*commands.Command
	for _, name := range names {
		if cmd := m.cmdRegistry.Find(name); cmd != nil {
			seen := false
			for _, existing := range matches {
				if existing.Name == cmd.Name {
					seen = true
					break
				}
			}
			if !seen {
				matches = append(matches, cmd)
			}
		}
	}

	if len(matches) > 8 {
		matches = matches[:8]
	}
	m.slashMatches = matches
	m.slashMenuOpen = len(matches) > 0
	if m.slashCursor >= len(matches) {
		m.slashCursor = 0
	}
}

func (m *Model) updateAtMenu() {
	if m.slashMenuOpen {
		m.atMenuOpen = false
		return
	}

	text := m.textarea.Value()
	atIdx := strings.LastIndex(text, "@")
	if atIdx < 0 {
		m.atMenuOpen = false
		m.atMatches = nil
		m.atCursor = 0
		return
	}

	after := text[atIdx+1:]
	if strings.Contains(after, " ") {
		m.atMenuOpen = false
		m.atMatches = nil
		m.atCursor = 0
		return
	}

	m.atPrefix = after
	matches := scanFiles(after, 8)
	m.atMatches = matches
	m.atMenuOpen = len(matches) > 0
	if m.atCursor >= len(matches) {
		m.atCursor = 0
	}
}

func scanFiles(prefix string, limit int) []string {
	dir := "."
	searchPrefix := prefix

	if strings.Contains(prefix, "/") {
		lastSlash := strings.LastIndex(prefix, "/")
		dir = prefix[:lastSlash]
		if dir == "" {
			dir = "/"
		}
		searchPrefix = prefix[lastSlash+1:]
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, ".venv": true,
		"__pycache__": true, ".zebracode": true, "vendor": true,
	}

	var matches []string
	for _, e := range entries {
		if skipDirs[e.Name()] {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") && searchPrefix == "" {
			continue
		}
		if searchPrefix != "" && !strings.HasPrefix(strings.ToLower(e.Name()), strings.ToLower(searchPrefix)) {
			continue
		}

		path := e.Name()
		if dir != "." {
			path = dir + "/" + e.Name()
		}
		if e.IsDir() {
			path += "/"
		}
		matches = append(matches, path)
		if len(matches) >= limit {
			break
		}
	}
	return matches
}
