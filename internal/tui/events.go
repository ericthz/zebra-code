package tui

import (
	"fmt"
	"time"

	"github.com/ericthz/zebra-code/internal/agent"
	"github.com/ericthz/zebra-code/internal/agents"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/tools"

	tea "github.com/charmbracelet/bubbletea"
)

type askUserMsg struct {
	req tools.AskUserRequest
}

type subAgentProgressMsg struct {
	progress agents.SubAgentProgress
}

func (m *Model) drainSubAgentProgress() {
	for {
		select {
		case p := <-m.subAgentProgressCh:
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
		default:
			return
		}
	}
}

func (m Model) listenForSubAgentProgress() tea.Cmd {
	ch := m.subAgentProgressCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		p := <-ch
		return subAgentProgressMsg{progress: p}
	}
}

func (m Model) listenForAskUser() tea.Cmd {
	ch := m.askUserCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		req := <-ch
		return askUserMsg{req: req}
	}
}

func (m Model) listenForAgentEvents() tea.Cmd {
	ch := m.agentCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return agentDoneMsg{ch: ch}
		}
		return agentEventMsg{event: ev, ch: ch}
	}
}

func (m Model) pollMailbox() tea.Cmd {
	if m.teamMgr == nil {
		return nil
	}
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return mailboxPollMsg{}
	})
}

func (m Model) handleAgentEvent(ev agent.AgentEvent) (tea.Model, tea.Cmd) {
	switch e := ev.(type) {
	case agent.StreamText:
		m.streamBuf += e.Text
		m.updateViewport()

	case agent.ToolUseEvent:
		if e.Args != nil {
			found := false
			for i := range m.toolBlocks {
				if m.toolBlocks[i].toolName == e.ToolName && m.toolBlocks[i].args == nil {
					m.toolBlocks[i].args = e.Args
					found = true
					break
				}
			}
			if !found {
				m.toolBlocks = append(m.toolBlocks, toolBlockInfo{
					toolName: e.ToolName,
					args:     e.Args,
					loading:  true,
				})
			}
		} else {
			m.toolBlocks = append(m.toolBlocks, toolBlockInfo{
				toolName: e.ToolName,
				loading:  true,
			})
		}
		m.updateViewport()

	case agent.ToolResultEvent:
		var emitted *toolBlockInfo
		for i := range m.toolBlocks {
			if m.toolBlocks[i].toolName == e.ToolName && m.toolBlocks[i].loading {
				m.toolBlocks[i].output = e.Output
				m.toolBlocks[i].isError = e.IsError
				m.toolBlocks[i].elapsed = e.Elapsed.Seconds()
				m.toolBlocks[i].loading = false
				m.toolBlocks[i].collapsed = true
				// Agent tools defer to TurnComplete so we can merge sub-agent progress.
				if m.toolBlocks[i].toolName != "Agent" {
					tb := m.toolBlocks[i]
					emitted = &tb
					m.toolBlocks = append(m.toolBlocks[:i], m.toolBlocks[i+1:]...)
				}
				break
			}
		}
		if emitted != nil {
			// Flush pending assistant text first so order matches Zebra Code's
			// per-block stream: text → tool result → text → tool result.
			if m.streamBuf != "" {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:    "assistant",
					content: m.streamBuf,
				})
				m.streamBuf = ""
			}
			m.chatMessages = append(m.chatMessages, chatMessage{
				role:      "tool_visible",
				content:   renderToolBlockText(*emitted),
				toolGroup: []toolBlockInfo{*emitted},
			})
			commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
			m.committedUpTo = len(m.chatMessages)
			m.updateViewport()
			if commitText != "" {
				return m, tea.Batch(tea.Println(commitText), m.listenForAgentEvents())
			}
		}
		m.updateViewport()

	case agent.TurnComplete:
		if m.streamBuf != "" {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: m.streamBuf})
			m.streamBuf = ""
		}
		// Drain any buffered sub-agent progress events
		m.drainSubAgentProgress()
		if len(m.toolBlocks) > 0 {
			var nonAgentTools []toolBlockInfo
			for _, tb := range m.toolBlocks {
				if tb.toolName == "Agent" && m.activeSubAgent != nil && len(m.activeSubAgent.toolUses) > 0 {
					// Finalize sub-agent block using collected progress
					sab := *m.activeSubAgent
					if !sab.done {
						sab.done = true
						sab.toolCount = len(sab.toolUses)
						var total float64
						for _, tu := range sab.toolUses {
							total += tu.elapsed
						}
						sab.totalTime = total
					}
					m.chatMessages = append(m.chatMessages, chatMessage{
						role:          "sub_agent",
						subAgentBlock: &sab,
						expanded:      false,
					})
					m.activeSubAgent = nil
				} else if tb.toolName == "Agent" {
					// Agent tool ran but no progress collected — extract info from result
					desc := ""
					if d, ok := tb.args["description"].(string); ok {
						desc = d
					}
					agentType := "general-purpose"
					if at, ok := tb.args["subagent_type"].(string); ok {
						agentType = at
					}
					sab := &subAgentBlock{
						desc:      desc,
						agentType: agentType,
						done:      true,
						totalTime: tb.elapsed,
						toolCount: 0,
					}
					m.chatMessages = append(m.chatMessages, chatMessage{
						role:          "sub_agent",
						subAgentBlock: sab,
						expanded:      false,
					})
					m.activeSubAgent = nil
				} else {
					nonAgentTools = append(nonAgentTools, tb)
				}
			}
			// Classify non-agent tools: visible (write/command) vs collapsed (read)
			var visibleTools, collapsedTools []toolBlockInfo
			for _, tb := range nonAgentTools {
				if isCollapsibleTool(tb.toolName) {
					collapsedTools = append(collapsedTools, tb)
				} else {
					visibleTools = append(visibleTools, tb)
				}
			}
			// Visible tools: show each as individual line
			for _, tb := range visibleTools {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:      "tool_visible",
					content:   renderToolBlockText(tb),
					toolGroup: []toolBlockInfo{tb},
				})
			}
			// Collapsed reads: hidden by default, shown on ctrl+o
			if len(collapsedTools) > 0 {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:      "tool_collapsed",
					toolGroup: collapsedTools,
					expanded:  false,
				})
			}
		}
		m.toolBlocks = nil
		m.activeSubAgent = nil
		m.updateViewport()

	case agent.UsageEvent:
		m.totalInput = e.InputTokens
		m.totalOutput = e.OutputTokens

	case agent.PermissionRequestEvent:
		m.permDialog = true
		m.permCursor = 0
		m.permToolName = e.ToolName
		m.permDesc = e.Desc
		m.permRespCh = e.ResponseCh
		m.updateViewport()
		return m, nil

	case agent.CompactEvent:
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: "⟳ " + e.Message,
		})
		m.updateViewport()

	case agent.RetryEvent:
		msg := "↻ Retrying: " + e.Reason
		if e.Wait > 0 {
			msg += fmt.Sprintf(" (waiting %s)", e.Wait)
		}
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: msg,
		})
		m.updateViewport()

	case agent.ErrorEvent:
		// 保留错误前已输出的流式文本
		if m.streamBuf != "" {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: m.streamBuf})
			m.streamBuf = ""
		}
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: e.Message,
		})
		commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
		m.committedUpTo = len(m.chatMessages)
		m.finishStreaming()
		if commitText != "" {
			return m, tea.Println(commitText)
		}
		return m, nil

	case agent.LoopComplete:
		totalTime := time.Since(m.thinkingStart).Seconds()
		if m.streamBuf != "" {
			// 助手消息由主循环在进入对话历史时落盘，这里只负责上屏
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: m.streamBuf})
			m.streamBuf = ""
		}
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "thinking",
			content: fmt.Sprintf("✻ %s for %.1fs", m.pastTense(m.thinkingVerb), totalTime),
		})
		commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
		m.committedUpTo = len(m.chatMessages)
		m.thinkingDone = 0
		m.finishStreaming()
		if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Mode == permissions.ModePlan {
			m.planApprovalDialog = true
			m.planApprovalCursor = 0
			m.planApprovalInput = ""
			m.updateViewport()
		}
		pollCmd := m.pollMailbox()
		if commitText != "" {
			return m, tea.Batch(tea.Println(commitText), pollCmd)
		}
		if pollCmd != nil {
			return m, pollCmd
		}
		return m, nil
	}

	return m, m.listenForAgentEvents()
}
