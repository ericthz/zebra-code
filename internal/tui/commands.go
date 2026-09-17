package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ericthz/zebra-code/internal/commands"
	"github.com/ericthz/zebra-code/internal/compact"
	"github.com/ericthz/zebra-code/internal/conversation"
	"github.com/ericthz/zebra-code/internal/filehistory"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/planfile"
	"github.com/ericthz/zebra-code/internal/prompt"
	"github.com/ericthz/zebra-code/internal/session"
	"github.com/ericthz/zebra-code/internal/skills"
	"github.com/ericthz/zebra-code/internal/teams"
	"github.com/ericthz/zebra-code/internal/todo"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func (m Model) buildCommandContext(args string) *commands.Context {
	wd, _ := os.Getwd()
	modelName := ""
	if m.selectedProvider != nil {
		modelName = m.selectedProvider.Model
	}
	return &commands.Context{
		Args:    args,
		WorkDir: wd,
		Model:   modelName,
		MemoryList: func() []string {
			if m.memoryMgr == nil {
				return nil
			}
			return m.memoryMgr.GetMemories()
		},
		MemoryClear: func() {
			if m.memoryMgr != nil {
				m.memoryMgr.Clear()
			}
		},
		TokenCount: func() (int, int) {
			return m.totalInput, m.totalOutput
		},
		PermissionMode: func() string {
			if m.ag != nil && m.ag.Checker != nil {
				return string(m.ag.Checker.Mode)
			}
			return "default"
		},
		SetPermissionMode: func(mode string) error {
			if m.ag == nil || m.ag.Checker == nil {
				return fmt.Errorf("permission system not initialized")
			}
			target := permissions.PermissionMode(mode)
			switch target {
			case permissions.ModeDefault, permissions.ModeAcceptEdits, permissions.ModePlan, permissions.ModeBypass:
				m.ag.Checker.Mode = target
				return nil
			default:
				return fmt.Errorf("invalid mode: %s (expected: default|acceptEdits|plan|bypassPermissions)", mode)
			}
		},
		ToolCount: func() int {
			return len(m.registry.ListTools())
		},
		SessionInfo: func() string {
			return fmt.Sprintf("Current session: %d messages", len(m.chatMessages))
		},
		SkillList: func() []commands.SkillInfo {
			if m.skillCatalog == nil {
				return nil
			}
			var result []commands.SkillInfo
			for _, meta := range m.skillCatalog.List() {
				result = append(result, commands.SkillInfo{
					Name:        meta.Name,
					Description: meta.Description,
				})
			}
			return result
		},
		SkillReload: func() int {
			if m.skillCatalog == nil {
				return 0
			}
			wd, _ := os.Getwd()
			m.skillCatalog.Reload(wd)
			for _, meta := range m.skillCatalog.List() {
				m.registerSkillCommand(meta.Name)
			}
			if m.client != nil {
				m.client.SetSystemPrompt(m.rebuildSystemPrompt(wd))
			}
			return len(m.skillCatalog.List())
		},
		MCPInfo: func() string {
			return m.mcpServerInfo
		},
	}
}

func (m Model) executeCommand(name, args string) (tea.Model, tea.Cmd) {
	cmd := m.cmdRegistry.Find(name)
	if cmd == nil {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: fmt.Sprintf("Unknown command: /%s — type /help to see available commands", name),
		})
		m.updateViewport()
		return m, nil
	}

	if args == "" && cmd.ArgPrompt != "" {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: cmd.ArgPrompt,
		})
		m.updateViewport()
		return m, nil
	}

	ctx := m.buildCommandContext(args)

	switch cmd.Type {
	case commands.TypeLocalUI:
		switch name {
		case "clear":
			m.chatMessages = nil
			m.committedUpTo = 0
			m.conversation = conversation.NewManager()
			if m.ag != nil {
				m.ag.ClearActiveSkills()
				// Skill 的工具收窄随对话一起清掉，但 coordinator 的约束不清：
				// 它跟着 Team 走，Team 还在就该继续管着 Lead。
				m.ag.SetToolFilter(teams.CoordinatorToolFilter(m.EnableCoordinatorMode))
			}
			// 开启全新会话：重置 session ID 及关联的持久化存储
			wd, _ := os.Getwd()
			m.sessionID = session.NewID()
			m.fileHistory = filehistory.New(wd, m.sessionID)
			m.defaultTools.EditFile.FileHistory = m.fileHistory
			m.defaultTools.WriteFile.FileHistory = m.fileHistory
			if m.ag != nil {
				m.ag.FileHistory = m.fileHistory
				m.ag.SetSessionID(m.sessionID)
			}
			store := todo.NewStore(wd, m.sessionID)
			m.todoList = todo.NewTaskList(store)
			// 重置 token 计数
			m.totalInput = 0
			m.totalOutput = 0
			m.updateViewport()
			return m, tea.Batch(
				func() tea.Msg { return tea.ClearScreen() },
				tea.Println(m.renderBanner()+"\n"),
			)
		case "plan":
			wd, _ := os.Getwd()
			if m.ag != nil && m.ag.Checker != nil {
				m.prePlanMode = m.ag.Checker.Mode
				m.ag.Checker.Mode = permissions.ModePlan
				planPath := planfile.GetOrCreatePlanPath(wd)
				m.ag.Checker.PlanFilePath = planPath
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:    "system",
					content: fmt.Sprintf("Entered Plan mode. Plan file: %s\nExplore the codebase and design your approach.", planPath),
				})

				// 重入检测：如果本次会话曾退出过 Plan Mode 且 plan 文件已存在，注入重入提示
				if m.hasExitedPlanMode && planfile.PlanExists(wd) {
					reentryMsg := prompt.BuildPlanModeReentryReminder(planPath, true)
					if reentryMsg != "" {
						m.chatMessages = append(m.chatMessages, chatMessage{
							role:    "system",
							content: reentryMsg,
						})
					}
					m.hasExitedPlanMode = false
				}
			}
			if args != "" {
				m.updateViewport()
				return m.sendMessage(args)
			}
			m.updateViewport()
			return m, nil
		case "compact":
			if m.client == nil || m.conversation == nil {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role: "error", content: "Compact requires an active provider.",
				})
				m.updateViewport()
				return m, nil
			}
			m.chatMessages = append(m.chatMessages, chatMessage{
				role: "system", content: "Compacting conversation…",
			})
			m.updateViewport()
			client := m.client
			conv := m.conversation
			window := 200000
			if m.selectedProvider != nil {
				window = m.selectedProvider.GetContextWindow()
			}
			var recovery *compact.RecoveryState
			var schemas []map[string]any
			if m.ag != nil {
				recovery = m.ag.RecoveryState
				schemas = m.ag.Registry.GetAllSchemas(m.ag.Protocol)
			}
			compactWD, _ := os.Getwd()
			compactSessionID := m.sessionID
			return m, func() tea.Msg {
				msg, err := compact.ForceCompact(context.Background(), conv, client, compactWD, compactSessionID, window, recovery, schemas)
				return compactDoneMsg{message: msg, err: err}
			}
		case "resume":
			return m.handleResume(args)
		case "rewind":
			return m.handleRewind()
		case "sandbox":
			m.sandboxDialog = true
			m.sandboxCursor = 0
			m.updateViewport()
			return m, nil
		}

	case commands.TypePrompt:
		if cmd.Handler != nil {
			prompt := cmd.Handler(ctx)
			displayText := "/" + name
			if args != "" {
				displayText += " " + args
			}
			m.updateViewport()
			newModel, teaCmd := m.sendPromptCommand(displayText, prompt)
			if strings.HasSuffix(cmd.Description, "[skill]") {
				loadedLine := lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(
					fmt.Sprintf("skill(%s)\nSuccessfully loaded skill", name))
				return newModel, tea.Batch(tea.Println(loadedLine), teaCmd)
			}
			return newModel, teaCmd
		}

	case commands.TypeSkillFork:
		// Fork-mode skill: run the skill in an isolated sub-agent, show a
		// progress notice in the main chat, and inject the final assistant
		// text once the sub-agent reports back. Off-thread so the TUI
		// stays responsive while the sub-agent thinks.
		//
		// The two header lines (user echo + "Forking…" notice) get committed
		// to terminal scrollback via tea.Println the same way sendMessage
		// commits its userLine — without this the viewport keeps growing
		// during the sub-agent run and pushes earlier history above the fold.
		displayText := "/" + name
		if args != "" {
			displayText += " " + args
		}
		m.chatMessages = append(m.chatMessages, chatMessage{role: "user", content: displayText})
		userLine := promptStyle.Render("❯ ") + lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(displayText)
		forkNotice := fmt.Sprintf("Forking skill %s in isolated sub-agent…", name)
		m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: forkNotice})
		m.committedUpTo = len(m.chatMessages)
		m.updateViewport()
		skillName := name
		skillArgs := args
		commitText := userLine + "\n" + lipgloss.NewStyle().Foreground(dimText).Render(forkNotice)
		return m, tea.Batch(
			tea.Println(commitText),
			func() tea.Msg {
				skill, err := m.skillCatalog.GetFull(skillName)
				if err != nil && skill == nil {
					return forkSkillDoneMsg{name: skillName, err: err}
				}
				if m.ag != nil {
					m.ag.RecoveryState.RecordSkillInvocation(skill.Meta.Name, skill.PromptBody)
				}
				result, runErr := skills.RunFork(context.Background(), skill, skillArgs, m)
				return forkSkillDoneMsg{name: skillName, result: result, err: runErr}
			},
		)

	case commands.TypeLocal:
		if cmd.Handler != nil {
			output := cmd.Handler(ctx)
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: output})
			m.updateViewport()
			return m, nil
		}
	}

	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: fmt.Sprintf("/%s — not yet implemented", name),
	})
	m.updateViewport()
	return m, nil
}
