package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/ericthz/zebra-code/internal/agent"
	"github.com/ericthz/zebra-code/internal/agents"
	"github.com/ericthz/zebra-code/internal/filehistory"
	"github.com/ericthz/zebra-code/internal/history"
	"github.com/ericthz/zebra-code/internal/hooks"
	"github.com/ericthz/zebra-code/internal/llm"
	"github.com/ericthz/zebra-code/internal/mcp"
	"github.com/ericthz/zebra-code/internal/memory"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/sandbox"
	"github.com/ericthz/zebra-code/internal/session"
	"github.com/ericthz/zebra-code/internal/teams"
	"github.com/ericthz/zebra-code/internal/tools"

	tea "github.com/charmbracelet/bubbletea"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.textarea.SetWidth(msg.Width - 4)

		statusHeight := 1
		sepHeight := 2 // top + bottom separators around input
		inputHeight := m.textarea.Height() + 1
		vpHeight := msg.Height - statusHeight - sepHeight - inputHeight - 1
		if vpHeight < 1 {
			vpHeight = 1
		}

		if !m.ready {
			m.viewport = viewport.New(msg.Width, vpHeight)
			m.viewport.MouseWheelEnabled = false
			m.ready = true
			m.bannerPrinted = true
			m.updateViewport()
			return m, tea.Println(m.renderBanner() + "\n")
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = vpHeight
		}
		m.updateViewport()
		return m, nil

	case subAgentProgressMsg:
		p := msg.progress
		if p.Done {
			if m.activeSubAgent != nil {
				m.activeSubAgent.done = true
				m.activeSubAgent.toolCount = p.ToolCount
				m.activeSubAgent.totalTime = p.TotalTime
			}
		} else {
			if m.activeSubAgent == nil || m.activeSubAgent.done {
				m.activeSubAgent = &subAgentBlock{
					desc:      p.AgentDesc,
					agentType: p.AgentType,
				}
			}
			m.activeSubAgent.toolUses = append(m.activeSubAgent.toolUses, toolBlockInfo{
				toolName: p.ToolName,
				args:     p.ToolArgs,
				elapsed:  p.Elapsed,
				isError:  p.IsError,
			})
		}
		m.updateViewport()
		return m, m.listenForSubAgentProgress()

	case askUserMsg:
		m.askUserDialog = true
		m.askUserQuestions = msg.req.Questions
		m.askUserRespCh = msg.req.ResponseCh
		m.askUserQIdx = 0
		m.askUserCursors = make([]int, len(msg.req.Questions))
		m.askUserSelected = make([]map[int]bool, len(msg.req.Questions))
		m.askUserOther = make([]string, len(msg.req.Questions))
		m.askUserAnswered = make(map[int]string)
		m.askUserOnSubmit = false
		m.askUserSubmitIdx = 0
		for i := range msg.req.Questions {
			m.askUserSelected[i] = make(map[int]bool)
		}
		m.updateViewport()
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.streaming && m.cancelStream != nil {
				m.cancelStream()
				m.savePartialResponse()
				m.finishStreaming()
				return m, nil
			}
			if m.cancelStream != nil {
				m.cancelStream()
			}
			if m.mcpMgr != nil {
				m.mcpMgr.Shutdown()
			}
			if m.memoryExtractor != nil {
				_ = m.memoryExtractor.Drain(5000)
			}
			return m, tea.Quit
		}

		if m.permDialog {
			return m.handlePermDialog(msg)
		}

		switch m.state {
		case stateProviderSelect:
			return m.handleProviderSelect(msg)
		case stateChat:
			return m.handleChat(msg)
		case stateResume:
			return m.handleResumeKeys(msg)
		}

	case initSingleProviderMsg:
		p := &m.providers[0]
		m.selectedProvider = p
		wd, _ := os.Getwd()
		systemPrompt := m.loadSkillsAndBuildPrompt(wd)
		client, err := llm.NewClient(p, systemPrompt)
		if err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: err.Error()})
			m.updateViewport()
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
		pathSandbox := permissions.NewPathSandbox(wd, sandboxAllow...)
		ag.Checker = permissions.NewChecker(
			pathSandbox,
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
					AllowWrite:     pathSandbox.GetAllowedRoots(),
					DenyWrite:      pathSandbox.GetDenyWrite(),
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
		m.textarea.Focus()
		m.updateViewport()
		return m, m.initMCPServersCmd()

	case forkSkillDoneMsg:
		var commit string
		if msg.err != nil {
			line := fmt.Sprintf("Skill %s (fork) failed: %v", msg.name, msg.err)
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: line})
			commit = errorStyle.Render("✖ " + line)
		} else {
			result := strings.TrimSpace(msg.result)
			if result == "" {
				result = fmt.Sprintf("(Skill %s returned no output)", msg.name)
			}
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: result})
			commit = m.renderMessagesRange(len(m.chatMessages)-1, len(m.chatMessages))
		}
		m.committedUpTo = len(m.chatMessages)
		m.updateViewport()
		if commit != "" {
			return m, tea.Println(commit)
		}
		return m, nil

	case compactDoneMsg:
		switch {
		case msg.err != nil:
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: "Compact failed: " + msg.err.Error()})
		case msg.message == "":
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "Compact: no changes."})
		default:
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "Compact: " + msg.message})
		}
		m.updateViewport()
		return m, nil

	case mcpReadyMsg:
		m.mcpConnecting = false
		m.mcpMgr = msg.result.Mgr
		for _, t := range msg.result.Tools {
			m.registry.Register(t)
		}
		var mcpPrintLines []string
		for _, errMsg := range msg.result.Errors {
			m.chatMessages = append(m.chatMessages, chatMessage{
				role:    "error",
				content: errMsg,
			})
			mcpPrintLines = append(mcpPrintLines, errorStyle.Render("✖ "+errMsg))
		}
		registered := len(msg.result.Tools)
		if registered > 0 {
			m.mcpServerInfo = fmt.Sprintf("Connected to %d MCP server(s), %d tools registered", len(m.mcpConfigs)-len(msg.result.Errors), registered)
		}
		m.committedUpTo = len(m.chatMessages)
		// Build MCP instructions for system prompt injection
		if len(msg.result.Servers) > 0 {
			// Group registered tool names by server
			toolsByServer := make(map[string][]string)
			for _, t := range msg.result.Tools {
				toolName := t.Name()
				for _, srv := range msg.result.Servers {
					if strings.HasPrefix(toolName, "mcp__"+mcp.SanitizeName(srv.Name)+"__") {
						toolsByServer[srv.Name] = append(toolsByServer[srv.Name], toolName)
						break
					}
				}
			}

			var mcpParts []string
			for _, srv := range msg.result.Servers {
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("## %s\n", srv.Name))
				if srv.Instructions != "" {
					sb.WriteString(srv.Instructions + "\n")
				}
				if toolNames, ok := toolsByServer[srv.Name]; ok && len(toolNames) > 0 {
					sb.WriteString("\nAvailable tools: " + strings.Join(toolNames, ", "))
				}
				mcpParts = append(mcpParts, sb.String())
			}
			m.mcpInstructions = "# MCP Server Instructions\n\nThe following MCP servers are connected. Use their tools when the user asks.\n\n" + strings.Join(mcpParts, "\n\n")
		}
		m.updateViewport()
		if len(mcpPrintLines) > 0 {
			return m, tea.Println(strings.Join(mcpPrintLines, "\n"))
		}
		return m, nil

	case spinner.TickMsg:
		if m.streaming {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			m.updateViewport()
			return m, cmd
		}

	case agentReadyMsg:
		m.agentCh = msg.ch
		return m, m.listenForAgentEvents()

	case agentEventMsg:
		if msg.ch != m.agentCh {
			return m, nil
		}
		return m.handleAgentEvent(msg.event)

	case agentDoneMsg:
		if msg.ch != m.agentCh {
			return m, nil
		}
		m.finishStreaming()
		return m, nil

	case agentErrMsg:
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: msg.err.Error(),
		})
		m.finishStreaming()
		return m, nil

	case mailboxPollMsg:
		if m.streaming {
			return m, nil
		}
		notifications := teams.DrainLeadMailbox(m.teamMgr)
		if len(notifications) == 0 {
			return m, m.pollMailbox()
		}
		for _, note := range notifications {
			m.conversation.AddSystemReminder(note)
		}
		m.streaming = true
		m.thinking = true
		m.thinkingStart = time.Now()
		m.thinkingDone = 0
		m.thinkingVerb = randomVerb()
		m.streamBuf = ""
		m.toolBlocks = nil
		m.userScrolled = false
		ctx, cancel := context.WithCancel(context.Background())
		m.cancelStream = cancel
		m.agentCh = m.ag.Run(ctx, m.conversation)
		m.updateViewport()
		return m, tea.Batch(
			m.listenForAgentEvents(),
			m.listenForAskUser(),
			m.listenForSubAgentProgress(),
			m.spinner.Tick,
		)
	}

	return m, nil
}
