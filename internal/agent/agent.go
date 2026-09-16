package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ericthz/zebra-code/internal/compact"
	"github.com/ericthz/zebra-code/internal/conversation"
	"github.com/ericthz/zebra-code/internal/filehistory"
	"github.com/ericthz/zebra-code/internal/hooks"
	"github.com/ericthz/zebra-code/internal/llm"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/planfile"
	"github.com/ericthz/zebra-code/internal/prompt"
	"github.com/ericthz/zebra-code/internal/session"
	"github.com/ericthz/zebra-code/internal/toolresult"
	"github.com/ericthz/zebra-code/internal/tools"
)

const (
	maxTokensCeiling          = 64000
	maxOutputTokensRecoveries = 3
)

type Agent struct {
	Client        llm.Client
	Registry      *tools.Registry
	Protocol      string
	WorkDir       string
	MaxIterations int
	ContextWindow int
	// MaxOutputTokens is the model's max output budget; used by Layer 2 to
	// compute the effective window for the compaction threshold. Zero falls
	// back to the summaryOutputReserve default inside compact.
	MaxOutputTokens int
	Checker         *permissions.Checker
	Hooks           *hooks.Engine
	// SessionID identifies the on-disk session log this agent appends to. When
	// set, Layer 2 compaction writes a compact_boundary record into that session
	// so a later resume can rebuild the compacted state instead of replaying the
	// full pre-compaction transcript. Empty disables boundary persistence (tests,
	// one-shot callers).
	SessionID      string
	NotificationFn func() []string
	// ToolNameFilter, when non-nil, drops any tool whose Name returns false from the schemas sent to
	// the LLM. The filter is consulted at the top of every iteration so callers can flip Coordinator
	// Mode on or off (e.g., when a team is created/torn down) without restarting the agent.
	Instructions  string
	MemoryContent string
	// MemoryRecallCh 非阻塞 memory recall：prefetch 与主 LLM 调用并行，
	// 工具执行后从 channel 读取并注入
	MemoryRecallCh <-chan string
	ToolNameFilter func(name string) bool
	// CoordinatorActiveFn, when non-nil, reports whether Coordinator Mode is currently in effect.
	// Consulted every iteration alongside ToolNameFilter so the scheduling guidance appears exactly
	// when the tool set is narrowed, and disappears once the team is torn down.
	CoordinatorActiveFn func() bool
	// OnLoopComplete, when non-nil, is invoked fire-and-forget after the agent reaches LoopComplete
	// (final assistant message, no tool calls remaining). Used by ch09 background memory extraction.
	// Replaces the original stopHooks dispatcher; failures are silent and must not block the main
	// loop. The callback receives the live conversation — do not mutate it from another goroutine.
	OnLoopComplete  func(conv *conversation.Manager)
	FileHistory     *filehistory.History
	compactTracking compact.AutoCompactTrackingState
	// RecoveryState holds the snapshots needed to rebuild working context
	// after Layer 2 collapses the conversation into a summary: most-recent
	// file reads and skill invocations. The struct is concurrency-safe so
	// the streaming executor can write to it from multiple goroutines.
	RecoveryState *compact.RecoveryState
	eventCh       chan AgentEvent
	// activeSkills tracks which Skill SOPs have been activated in this session (name → body).
	// Used by /skills to show what's active and by RecoveryState to survive compaction.
	// The body is injected once into the conversation as a message — NOT re-injected every turn.
	activeSkills map[string]string
}

// ActivateSkill records a skill activation. The body is kept for /skills listing and compaction
// recovery, but is NOT re-injected every turn — it lives in the conversation as a regular message.
func (a *Agent) ActivateSkill(name, body string) {
	if a.activeSkills == nil {
		a.activeSkills = make(map[string]string)
	}
	a.activeSkills[name] = body
}

// ClearActiveSkills drops every pinned SOP. Called by /clear so a fresh conversation doesn't carry
// over SOPs from a prior task. Safe to call when no skills were ever activated.
func (a *Agent) ClearActiveSkills() {
	a.activeSkills = nil
}

// GetActiveSkills returns a copy of the currently-pinned SOPs (name → body). Used by tests and by
// /skills to surface what's active.
func (a *Agent) GetActiveSkills() map[string]string {
	out := make(map[string]string, len(a.activeSkills))
	for k, v := range a.activeSkills {
		out[k] = v
	}
	return out
}

// SetToolFilter installs a tool visibility filter for the current conversation. The filter is
// consulted at the top of every iteration so callers can flip Coordinator mode on or off without
// restarting the agent. Passing nil clears any previous filter.
func (a *Agent) SetToolFilter(allow func(name string) bool) {
	a.ToolNameFilter = allow
}

// ToolRegistry returns the live tool registry. Named ToolRegistry (not just Registry, even though
// that would match the field name) to avoid the method/field collision Go disallows. Matches the
// skills.SkillHost contract.
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.Registry
}

func New(client llm.Client, registry *tools.Registry, protocol string) *Agent {
	wd, _ := os.Getwd()
	return &Agent{
		Client:        client,
		Registry:      registry,
		Protocol:      protocol,
		WorkDir:       wd,
		MaxIterations: 0,
		ContextWindow: 200000,
		RecoveryState: compact.NewRecoveryState(),
	}
}

// SetSessionID wires the on-disk session log id onto the agent so Layer 2
// compaction can persist a compact_boundary record into the same session the TUI
// is appending plain messages to. Called from the TUI right after the agent is
// constructed (and again after a resume switches sessions).
func (a *Agent) SetSessionID(id string) { a.SessionID = id }

// currentToolSchemas builds the schema list the next API call will use,
// honouring any active ToolNameFilter (e.g. Teams coordinator mode).
// Shared between the recovery attachment (which lists what's still
// available after compact) and the actual Stream call so both views
// stay consistent.
func (a *Agent) currentToolSchemas() []map[string]any {
	schemas := a.Registry.GetAllSchemas(a.Protocol)
	if a.ToolNameFilter == nil {
		return schemas
	}
	// 过滤器就是唯一依据，不留任何例外分支
	return filterSchemasByName(schemas, a.ToolNameFilter)
}

func (a *Agent) Run(ctx context.Context, conv *conversation.Manager) <-chan AgentEvent {
	ch := make(chan AgentEvent, 32)

	go func() {
		defer close(ch)
		defer a.emitHook(hooks.EventSessionEnd, "", nil)

		a.emitHook(hooks.EventSessionStart, "", nil)

		conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)

		var totalInput, totalOutput int
		maxTokensEscalated := false
		outputRecoveries := 0

		for iteration := 1; ; iteration++ {
			if a.MaxIterations > 0 && iteration > a.MaxIterations {
				ch <- ErrorEvent{Message: fmt.Sprintf("Agent reached maximum iterations (%d)", a.MaxIterations)}
				return
			}

			if ctx.Err() != nil {
				return
			}

			// Compute the tool schema list once per iteration so the recovery
			// attachment (when compact fires) and the actual Stream call below
			// agree on what's wired up. Skill filters can only change between
			// iterations, never within one.
			toolSchemas := a.currentToolSchemas()

			// Plan mode: inject structured workflow reminder.
			if a.Checker != nil && a.Checker.Mode == permissions.ModePlan {
				planPath := planfile.GetOrCreatePlanPath(a.WorkDir)
				a.Checker.PlanFilePath = planPath
				planExists := planfile.PlanExists(a.WorkDir)
				reminder := prompt.BuildPlanModeReminder(planPath, planExists, iteration)
				conv.AddSystemReminder(reminder)
			}

			// Coordinator 模式：工具集被收窄的同时注入调度指引。
			// 走 system-reminder 而不是系统提示词：长会话里开头那份约束会被淹没，
			// 每轮追加一次才拉得回来，而且系统提示词是缓存前缀，动它整段都要重新计费。
			if a.CoordinatorActiveFn != nil && a.CoordinatorActiveFn() {
				conv.AddSystemReminder(prompt.CoordinatorReminder(iteration))
			}

			if a.NotificationFn != nil {
				for _, note := range a.NotificationFn() {
					conv.AddSystemReminder(note)
				}
			}

			a.emitHook(hooks.EventTurnStart, "", nil)

			// Inject deferred tool names into system-reminder so the model knows what's available via
			// ToolSearch.
			if deferredNames := a.Registry.GetDeferredToolNames(); len(deferredNames) > 0 {
				reminder := "The following deferred tools are available via ToolSearch. Their schemas are NOT loaded - use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n" + strings.Join(deferredNames, "\n")
				conv.AddSystemReminder(reminder)
			}

			a.emitHook(hooks.EventPreSend, "", nil)

			// Layer 2: auto-compact
			// Layer 1（工具结果预算）在结果入历史时已处理完，历史里的
			// 内容就是最终大小，直接用其消息估算 token
			if msg, err := compact.ManageContext(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.MaxOutputTokens, &a.compactTracking, a.RecoveryState, toolSchemas); err == nil && msg != "" {
				ch <- CompactEvent{Message: msg}
				conv.ClearUsageAnchor()
				conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)
			}

			events, errs := a.Client.Stream(ctx, conv, toolSchemas)

			var text string
			var toolCalls []llm.ToolCallComplete
			var thinkingBlocks []conversation.ThinkingBlock
			var stopReason string
			var usage llm.UsageInfo

			executor := NewStreamingExecutor(a.Registry, ch)

			for ev := range events {
				switch e := ev.(type) {
				case llm.ThinkingDelta:
					ch <- ThinkingText{Text: e.Text}
				case llm.ThinkingComplete:
					thinkingBlocks = append(thinkingBlocks, conversation.ThinkingBlock{
						Thinking:  e.Thinking,
						Signature: e.Signature,
					})
				case llm.TextDelta:
					text += e.Text
					ch <- StreamText{Text: e.Text}
				case llm.ToolCallStart:
					ch <- ToolUseEvent{ToolID: e.ToolID, ToolName: e.ToolName}
				case llm.ToolCallDelta:
					// ignore
				case llm.ToolCallComplete:
					toolCalls = append(toolCalls, e)
					ch <- ToolUseEvent{
						ToolID:   e.ToolID,
						ToolName: e.ToolName,
						Args:     e.Arguments,
					}
					// 收集工具调用，等流式结束后按安全性分批执行
					executor.Submit(toolCallInfo{
						toolID:    e.ToolID,
						toolName:  e.ToolName,
						arguments: e.Arguments,
					})
				case llm.StreamEnd:
					stopReason = e.StopReason
					usage = e.Usage
				}
			}
			a.emitHook(hooks.EventPostReceive, text, nil)

			// Handle stream errors.
			select {
			case err := <-errs:
				if err != nil {
					if retry, compacted := a.handleStreamError(ctx, ch, conv, err); retry {
						if compacted {
							conv.ClearUsageAnchor()
							conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)
						}
						continue // retry the turn
					}
					ch <- ErrorEvent{Message: err.Error()}
					return
				}
			default:
			}

			totalInput += usage.InputTokens
			totalOutput += usage.OutputTokens
			ch <- UsageEvent{InputTokens: totalInput, OutputTokens: totalOutput}

			anchorAfterAssistant := func() {
				conv.RecordUsageAnchor(
					usage.InputTokens,
					usage.OutputTokens,
					usage.CacheReadTokens,
					usage.CacheCreationTokens,
				)
			}

			// Handle max_tokens stop reason.
			if stopReason == "max_tokens" {
				if !maxTokensEscalated {
					// First hit: escalate silently.
					if setter, ok := a.Client.(llm.MaxTokensSetter); ok {
						setter.SetMaxOutputTokens(maxTokensCeiling)
						maxTokensEscalated = true
					}
					if text != "" {
						conv.AddAssistantFull(text, thinkingBlocks, nil)
						a.persistLastMessage(conv)
						anchorAfterAssistant()
						conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Do not apologize or repeat previous content. Pick up mid-thought if needed.")
					}
					ch <- RetryEvent{Reason: "max_tokens escalation", Wait: 0}
					continue
				} else if outputRecoveries < maxOutputTokensRecoveries {
					// Multi-turn recovery.
					outputRecoveries++
					conv.AddAssistantFull(text, thinkingBlocks, nil)
					a.persistLastMessage(conv)
					anchorAfterAssistant()
					conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Break remaining work into smaller pieces.")
					ch <- RetryEvent{Reason: fmt.Sprintf("max_tokens recovery %d/%d", outputRecoveries, maxOutputTokensRecoveries), Wait: 0}
					continue
				}
				// Exhausted: fall through to normal completion.
			} else {
				// Reset recovery counter on successful turn.
				outputRecoveries = 0
			}

			if len(toolCalls) == 0 {
				conv.AddAssistantFull(text, thinkingBlocks, nil)
				a.persistLastMessage(conv)
				if a.FileHistory != nil {
					summary := text
					if len(summary) > 60 {
						summary = summary[:60] + "..."
					}
					a.FileHistory.MakeSnapshot(conv.Len(), summary)
				}
				ch <- LoopComplete{TotalTurns: iteration}
				if a.OnLoopComplete != nil {
					go a.OnLoopComplete(conv)
				}
				return
			}

			var toolUses []conversation.ToolUseBlock
			for _, tc := range toolCalls {
				toolUses = append(toolUses, conversation.ToolUseBlock{
					ToolUseID: tc.ToolID,
					ToolName:  tc.ToolName,
					Arguments: tc.Arguments,
				})
			}
			conv.AddAssistantFull(text, thinkingBlocks, toolUses)
			a.persistLastMessage(conv)
			// Anchor real usage to the conversation now that the assistant message
			// is in place; subsequent tool results + next user message are
			// estimated incrementally on top of this baseline.
			anchorAfterAssistant()

			// 按安全性分批执行：只读工具并发，写/命令工具串行
			results := executor.ExecuteAll(ctx, a)

			// 溢写文件的回读结果豁免溢写：把模型刚读回来的内容再写盘换成
			// 预览，模型就永远看不到全文，还会在「读回、溢写」之间打转。
			exempt := make(map[string]bool)
			for _, tc := range toolCalls {
				if toolresult.IsSpillReadback(tc.ToolName, tc.Arguments, a.WorkDir, a.SessionID) {
					exempt[tc.ToolID] = true
				}
			}

			var toolResults []conversation.ToolResultBlock
			for _, r := range results {
				ch <- ToolResultEvent{
					ToolID:   r.toolID,
					ToolName: r.toolName,
					Output:   r.output,
					IsError:  r.isError,
					Elapsed:  r.elapsed,
				}

				content := r.output
				if len(content) > tools.MaxOutputChars && !exempt[r.toolID] {
					// 单条超限：写盘换预览。写盘失败会原样保留，同一块磁盘
					// 聚合预算也不必再试，所以两种结果都标记豁免。
					content = toolresult.PersistLargeResult(a.WorkDir, a.SessionID, r.toolID, r.output)
					exempt[r.toolID] = true
				}
				toolResults = append(toolResults, conversation.ToolResultBlock{
					ToolUseID: r.toolID,
					Content:   content,
					IsError:   r.isError,
				})
			}

			// 聚合预算：一轮并行工具的结果落在同一条消息里，单条阈值管不住
			// 合计超限的情况。进历史前把整批处理完，消息一出生就是终态。
			toolresult.ApplyBudget(toolResults, exempt, a.WorkDir, a.SessionID)

			exitPlanCalled := false
			for _, tc := range toolCalls {
				if tc.ToolName == "ExitPlanMode" {
					exitPlanCalled = true
					break
				}
			}
			conv.AddToolResultsMessage(toolResults)
			a.persistLastMessage(conv)

			// 非阻塞 memory recall：工具执行完后检查 prefetch 是否就绪
			if a.MemoryRecallCh != nil {
				select {
				case recall := <-a.MemoryRecallCh:
					if recall != "" {
						conv.AddSystemReminder(recall)
					}
					a.MemoryRecallCh = nil // 只消费一次
				default:
					// prefetch 还没好，下轮再检查
				}
			}

			if exitPlanCalled {
				ch <- TurnComplete{Turn: iteration}
				ch <- LoopComplete{TotalTurns: iteration}
				return
			}
			ch <- TurnComplete{Turn: iteration}
			a.emitHook(hooks.EventTurnEnd, "", nil)
		}
	}()

	return ch
}

// emitHook fires a hook event when an Engine is configured. Failures are non-fatal and surface via
// the hook notification queue (drained into the next turn's system reminders).
func (a *Agent) emitHook(event hooks.EventName, message string, args map[string]any) {
	if a.Hooks == nil {
		return
	}
	a.Hooks.RunHooks(hooks.HookContext{
		EventName: event,
		ToolArgs:  args,
		Message:   message,
	})
}

// filterSchemasByName keeps only the tool schemas whose "name" passes the allow predicate. Used by
// Coordinator Mode to restrict a Lead agent to coordination-only tools while teammates do the
// actual work.
func filterSchemasByName(schemas []map[string]any, allow func(name string) bool) []map[string]any {
	out := make([]map[string]any, 0, len(schemas))
	for _, s := range schemas {
		name, _ := s["name"].(string)
		if allow(name) {
			out = append(out, s)
		}
	}
	return out
}

// handleStreamError returns (retry, compacted): retry signals the caller to
// re-run the turn; compacted signals that a ForceCompact rewrote the
// conversation, so the caller must drop its usage anchor (its AnchorCount no
// longer maps to the new transcript).
func (a *Agent) handleStreamError(ctx context.Context, ch chan AgentEvent, conv *conversation.Manager, err error) (retry, compacted bool) {
	var ctxErr *llm.ContextTooLongError
	if errors.As(err, &ctxErr) {
		// 历史里的工具结果在入历史时已按预算处理为终态，直接传 nil
		// 让 ForceCompact 使用 conv 自身消息
		msg, compactErr := compact.ForceCompact(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.RecoveryState, a.currentToolSchemas())
		if compactErr == nil && msg != "" {
			ch <- CompactEvent{Message: "Auto-compacted due to context length: " + msg}
			return true, true // retry, and the anchor is now stale
		}
		return false, false
	}

	var rlErr *llm.RateLimitError
	if errors.As(err, &rlErr) {
		wait := parseRetryAfter(rlErr.RetryAfter)
		ch <- RetryEvent{Reason: "rate limited", Wait: wait}
		select {
		case <-time.After(wait):
			return true, false // retry without compaction
		case <-ctx.Done():
			return false, false
		}
	}

	return false, false
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 5 * time.Second
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 5 * time.Second
}

type toolExecResult struct {
	toolID   string
	toolName string
	output   string
	isError  bool
	elapsed  time.Duration
}

// extractFilePath pulls a representative path from common tool argument keys so hooks can do path-
// glob matching (`file_path =* "**/*.go"`).
func extractFilePath(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "pattern", "target"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (a *Agent) executeSingleTool(ctx context.Context, eventCh chan AgentEvent, tc toolCallInfo) toolExecResult {
	tool := a.Registry.Get(tc.toolName)
	start := time.Now()

	if tool == nil {
		// 工具名不存在只回一条错误结果，让模型自己换个工具重来，不打断循环。
		return toolExecResult{
			toolID:   tc.toolID,
			toolName: tc.toolName,
			output:   fmt.Sprintf("Error: unknown tool '%s'", tc.toolName),
			isError:  true,
			elapsed:  time.Since(start),
		}
	}

	if a.Checker != nil {
		decision := a.Checker.Check(tool, tc.arguments)
		if decision.Effect == permissions.Deny {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   fmt.Sprintf("Permission denied: %s", decision.Reason),
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
		if decision.Effect == permissions.Ask {
			respCh := make(chan PermissionResponse, 1)
			desc := permissions.DescribeToolAction(tc.toolName, tc.arguments)
			eventCh <- PermissionRequestEvent{
				ToolName:   tc.toolName,
				Desc:       desc,
				ResponseCh: respCh,
			}
			resp := <-respCh
			if resp == PermDeny {
				return toolExecResult{
					toolID:   tc.toolID,
					toolName: tc.toolName,
					output:   conversation.RejectedToolResult,
					isError:  true,
					elapsed:  time.Since(start),
				}
			}
			if resp == PermAllowAlways {
				content := permissions.ExtractContent(tc.toolName, tc.arguments)
				pattern := content + "*"
				if len(content) > 60 {
					pattern = content[:60] + "*"
				}
				a.Checker.AddSessionAllow(tc.toolName, pattern)
				a.Checker.RuleEngine.AppendLocalRule(permissions.Rule{
					ToolName: tc.toolName,
					Pattern:  pattern,
					Effect:   permissions.RuleAllow,
				})
			}
		}
	}

	if a.Hooks != nil {
		hctx := hooks.HookContext{
			EventName: hooks.EventPreToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
		}
		if rejected, msg := a.Hooks.RunPreToolHooks(hctx); rejected {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   "Blocked by hook: " + msg,
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
	}

	result := tool.Execute(ctx, tc.arguments)

	if !result.IsError && tc.toolName == "ReadFile" {
		if p, _ := tc.arguments["file_path"].(string); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				a.RecoveryState.RecordFileRead(p, string(data))
			}
		}
	}

	if a.Hooks != nil {
		a.Hooks.RunHooks(hooks.HookContext{
			EventName: hooks.EventPostToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
			Message:   result.Output,
		})
	}

	return toolExecResult{
		toolID:   tc.toolID,
		toolName: tc.toolName,
		output:   result.Output,
		isError:  result.IsError,
		elapsed:  time.Since(start),
	}
}

func formatToolArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, s))
	}
	return strings.Join(parts, ", ")
}

// persistLastMessage 把刚追加进对话历史的那条消息写入会话日志。
//
// 落盘点放在主循环而不是各个前端：TUI 和 Web 共用同一条记录路径，
// 中间轮次的助手文本和完整的工具调用链都会被记下来，恢复会话时才能还原。
// WorkDir 或 SessionID 为空时（一次性调用、子 Agent）跳过，不写盘。
func (a *Agent) persistLastMessage(conv *conversation.Manager) {
	if a.WorkDir == "" || a.SessionID == "" {
		return
	}
	msgs := conv.GetMessages()
	if len(msgs) == 0 {
		return
	}
	session.SaveMessage(a.WorkDir, a.SessionID, session.FromConversation(msgs[len(msgs)-1]))
}
