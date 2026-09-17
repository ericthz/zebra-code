package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ericthz/zebra-code/internal/agent"
	"github.com/ericthz/zebra-code/internal/agents"
	"github.com/ericthz/zebra-code/internal/commands"
	"github.com/ericthz/zebra-code/internal/config"
	"github.com/ericthz/zebra-code/internal/conversation"
	"github.com/ericthz/zebra-code/internal/hooks"
	"github.com/ericthz/zebra-code/internal/llm"
	"github.com/ericthz/zebra-code/internal/mcp"
	"github.com/ericthz/zebra-code/internal/memory"
	"github.com/ericthz/zebra-code/internal/memory/consolidation"
	"github.com/ericthz/zebra-code/internal/memory/extractor"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/planfile"
	"github.com/ericthz/zebra-code/internal/prompt"
	"github.com/ericthz/zebra-code/internal/skills"
	"github.com/ericthz/zebra-code/internal/teams"
	"github.com/ericthz/zebra-code/internal/todo"
	"github.com/ericthz/zebra-code/internal/tools"
	"github.com/ericthz/zebra-code/internal/worktree"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) drainTaskNotifications() []string {
	var messages []string
	if m.taskMgr != nil {
		for _, n := range m.taskMgr.DrainNotifications() {
			msg := fmt.Sprintf("<task-notification>\n<task_id>%s</task_id>\n<status>%s</status>\n<summary>Agent \"%s\" %s</summary>\n<result>%s</result>\n</task-notification>",
				n.TaskID, n.Status, n.Name, n.Status, n.Output)
			messages = append(messages, msg)
		}
	}
	// Teammate idle notifications land in the lead's inbox; surface
	// them as system reminders so the Lead model sees them at the top
	// of the next turn and can dispatch follow-up work.
	messages = append(messages, teams.DrainLeadMailbox(m.teamMgr)...)
	// Hook notifications (post_tool_use output, async hook results, etc.)
	// drain into system reminders so the model sees side-effects.
	if m.ag != nil && m.ag.Hooks != nil {
		for _, r := range m.ag.Hooks.DrainNotifications() {
			if r.Output == "" || r.Output == "(async)" {
				continue
			}
			messages = append(messages, fmt.Sprintf("<hook-notification id=%q>\n%s\n</hook-notification>", r.HookID, r.Output))
		}
	}
	return messages
}

// newAgentHookRunner builds the AgentRunner closure used by `type: agent`
// hooks. The hook prompt is sent as a single user message to the same LLM
// the main agent uses, with no tool registry — output is the raw assistant
// text, which lands back in the notification queue and drains into the
// next turn's system reminders.

func newAgentHookRunner(client llm.Client) func(prompt string, ctx hooks.HookContext) (string, error) {
	return func(prompt string, _ hooks.HookContext) (string, error) {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conv := conversation.NewManager()
		conv.AddUserMessage(prompt)
		events, errs := client.Stream(c, conv, nil)
		var text string
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				text += td.Text
			}
		}
		select {
		case err := <-errs:
			if err != nil {
				return "", err
			}
		default:
		}
		return text, nil
	}
}

func (m *Model) registerAgentTools(client llm.Client, providerCfg *config.ProviderConfig, protocol, wd string) {
	m.taskMgr = agents.NewTaskManager()

	store := todo.NewStore(wd, m.sessionID)
	m.todoList = todo.NewTaskList(store)

	m.memoryMgr = memory.NewManager(wd)

	loader := agents.NewAgentLoader(wd)
	loader.LoadAll()

	teamMgr := teams.NewTeamManager()
	m.teamMgr = teamMgr

	// Wire worktree tools (T9: session restore, T13-T15: LLM tools, T17: cleanup)
	gitRoot := worktree.FindCanonicalGitRoot(wd)
	m.registry.Register(&tools.EnterWorktreeTool{
		SessionID: m.sessionID,
		RepoRoot:  gitRoot,
	})
	m.registry.Register(&tools.ExitWorktreeTool{
		RepoRoot: gitRoot,
	})

	// Restore worktree session from previous crash (T9)
	if gitRoot != "" {
		if savedSession, err := worktree.LoadWorktreeSession(gitRoot); err == nil && savedSession != nil {
			if info, err := os.Stat(savedSession.WorktreePath); err == nil && info.IsDir() {
				worktree.RestoreWorktreeSession(savedSession)
			}
		}
	}

	// Start background stale worktree cleanup (T17)
	worktree.StartCleanupLoop(context.Background())

	m.registry.Register(&tools.ExitPlanModeTool{
		IsPlanMode: func() bool {
			return m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Mode == permissions.ModePlan
		},
		PlanExists: func() bool {
			wd, _ := os.Getwd()
			return planfile.PlanExists(wd)
		},
	})
	m.registry.Register(&todo.TaskCreateTool{List: m.todoList})
	m.registry.Register(&todo.TaskGetTool{List: m.todoList})
	m.registry.Register(&todo.TaskListTool{List: m.todoList})
	m.registry.Register(&todo.TaskUpdateTool{List: m.todoList})
	m.registry.Register(&tools.ToolSearchTool{Registry: m.registry, Protocol: protocol})
	m.registry.Register(&teams.TeamCreateTool{TeamMgr: teamMgr})
	m.registry.Register(&teams.TeamDeleteTool{TeamMgr: teamMgr})
	m.registry.Register(&teams.SendMessageTool{TeamMgr: teamMgr, SenderName: "lead"})
	m.registry.Register(&teams.TaskStopTool{TeamMgr: teamMgr})
	m.registry.Register(&tools.SyntheticOutputTool{})
	m.registry.Register(&agents.AgentTool{
		Client:        client,
		ModelResolver: llm.NewModelResolver(*providerCfg),
		Registry:      m.registry,
		Protocol:      protocol,
		TaskMgr:       m.taskMgr,
		ProgressCh:    m.subAgentProgressCh,
		Loader:        loader,
		Conversation:  m.conversation,
		TeamMgr:       teamMgr,
		ForkDisabled:  m.ForkDisabled,
		// ParentChecker is wired below once m.ag.Checker is constructed —
		// registerAgentTools runs before the main agent's Checker is set.
	})

}

func (m *Model) initMCPServersCmd() tea.Cmd {
	if len(m.mcpConfigs) == 0 {
		return nil
	}

	m.mcpConnecting = true
	configs := m.mcpConfigs

	return func() tea.Msg {
		mgr := mcp.NewManager()
		var serverConfigs []mcp.ServerConfig
		for _, c := range configs {
			serverConfigs = append(serverConfigs, mcp.ServerConfig{
				Name:      c.Name,
				Command:   c.Command,
				Args:      c.Args,
				URL:       c.URL,
				Transport: c.Transport,
				Headers:   c.Headers,
				Env:       c.Env,
			})
		}
		mgr.LoadConfigs(serverConfigs)
		return mcpReadyMsg{result: mgr.ConnectAll(context.Background())}
	}
}

func (m *Model) loadSkillsAndBuildPrompt(wd string) string {
	m.skillCatalog = skills.LoadCatalog(wd)

	for _, cmd := range commands.LoadUserCommands(wd) {
		if m.cmdRegistry.HasConflict(cmd) {
			continue
		}
		m.cmdRegistry.Register(cmd)
	}

	return m.rebuildSystemPrompt(wd)
}

// wireSkillsToAgent finishes the Skill bring-up that loadSkillsAndBuildPrompt
// can't do because the Agent isn't constructed yet: registers per-Skill
// slash commands (inline vs fork-mode dispatch differs) and installs
// LoadSkillTool. Must be called immediately after m.ag is assigned and
// before the first user input is processed.
//
// Idempotent: silently skips a slash-command name that's already taken,
// matching the LoadUserCommands precedence in loadSkillsAndBuildPrompt and
// honoring the project rule "don't change the existing command set".

func (m *Model) wireSkillsToAgent() {
	if m.skillCatalog == nil || m.ag == nil {
		return
	}
	for _, meta := range m.skillCatalog.List() {
		m.registerSkillCommand(meta.Name)
	}
	m.registry.Register(&skills.LoadSkillTool{
		Catalog:  m.skillCatalog,
		Host:     m,
		ForkHost: m,
	})
	m.registry.Register(&skills.InstallSkillTool{
		Catalog: m.skillCatalog,
		OnInstalled: func(name string) {
			m.registerSkillCommand(name)
			if m.client != nil {
				wd, _ := os.Getwd()
				m.client.SetSystemPrompt(m.rebuildSystemPrompt(wd))
			}
		},
	})
}

// registerSkillCommand wires a single skill's slash command. Inline skills
// route through TypePrompt + skills.RunInline so the SOP gets pinned and
// allowed_tools filtering kicks in. Fork skills route through TypeSkillFork
// so the dispatcher can offload to a goroutine + sub-agent.
//
// Idempotent: returns silently if the command name is already taken.
// Extracted from wireSkillsToAgent so InstallSkillTool's OnInstalled hook
// can re-register a single newly-fetched skill without re-running the full
// startup loop.

func (m *Model) registerSkillCommand(name string) {
	if m.skillCatalog == nil || m.cmdRegistry == nil {
		return
	}
	if m.cmdRegistry.Find(name) != nil {
		return
	}
	meta := m.skillCatalog.Get(name)
	if meta == nil {
		return
	}
	cmd := &commands.Command{
		Name:        name,
		Description: meta.Meta.Description + " [skill]",
	}
	if meta.Meta.IsFork() {
		cmd.Type = commands.TypeSkillFork
		cmd.Handler = func(ctx *commands.Context) string {
			// Handler is unused for fork dispatch — executeCommand
			// branches on TypeSkillFork before calling Handler. Keep
			// it non-nil so legacy code paths that gate on Handler
			// presence still work.
			return ""
		}
	} else {
		cmd.Type = commands.TypePrompt
		captured := name
		cmd.Handler = func(ctx *commands.Context) string {
			skill, err := m.skillCatalog.GetFull(captured)
			if err != nil && skill == nil {
				return fmt.Sprintf("[skill error] %v", err)
			}
			body, runErr := skills.RunInline(context.Background(), skill, ctx.Args, m)
			if runErr != nil {
				return fmt.Sprintf("[skill error] %v", runErr)
			}
			if m.ag != nil {
				m.ag.RecoveryState.RecordSkillInvocation(skill.Meta.Name, body)
			}
			return body
		}
	}
	m.cmdRegistry.Register(cmd)
}

// refreshSkillsIfNeeded checks whether the skill directories have changed
// since the catalog was last loaded. If so, it reloads the catalog, registers
// any new slash commands, and updates the LLM client's system prompt so the
// model sees newly-added skills.

func (m *Model) refreshSkillsIfNeeded() {
	if m.skillCatalog == nil || m.client == nil {
		return
	}
	if !m.skillCatalog.NeedsReload() {
		return
	}
	wd, _ := os.Getwd()
	m.skillCatalog.Reload(wd)
	for _, meta := range m.skillCatalog.List() {
		m.registerSkillCommand(meta.Name)
	}
	m.client.SetSystemPrompt(m.rebuildSystemPrompt(wd))
}

// rebuildSystemPrompt regenerates the full system prompt from current state
// (skills, custom instructions, memory). Used by refreshSkillsIfNeeded and
// /skill reload.

func (m *Model) rebuildSystemPrompt(wd string) string {
	skillSection := m.buildSkillSection(wd)
	m.instructionsContent = m.loadCustomInstructions(wd)
	m.memoryContent = memory.LoadAutoMemoryPrompt(wd)
	env := prompt.DetectEnvironment(wd)
	if m.selectedProvider != nil {
		env.Model = m.selectedProvider.Model
	}
	// 指令与自动记忆由 conversation.InjectLongTermMemory 以 system-reminder
	// 消息注入对话，系统提示词这里只放 Skill。
	return prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		SkillSection: skillSection,
	})
}

// buildSkillSection generates the "## Available Skills" prompt section from
// the current catalog. Extracted from loadSkillsAndBuildPrompt so it can be
// reused by rebuildSystemPrompt.

func (m *Model) buildSkillSection(wd string) string {
	if m.skillCatalog == nil {
		return ""
	}
	metas := m.skillCatalog.List()
	if len(metas) == 0 {
		return ""
	}
	skillsDir := filepath.Join(wd, ".zebracode", "skills")
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString(fmt.Sprintf("Skills are installed at: %s\n", skillsDir))
	sb.WriteString("When creating new skills, always place them under this directory as <skill-name>/SKILL.md.\n\n")
	sb.WriteString("Only Skill names and one-line descriptions are listed below. To activate a Skill on demand call the LoadSkill tool with {name: \"<skill-name>\"}. After activation the Skill's full SOP gets pinned to the environment context, and any tools the Skill declares get registered. Users can also invoke a Skill directly with /<name>.\n\n")
	sb.WriteString("If the user pastes a Skill URL (skills.sh, github.com tree URL, or raw SKILL.md URL) and asks to install / add / get it, call the InstallSkill tool with {url: \"<url>\"} — the new Skill becomes available immediately afterwards.\n\n")
	for _, meta := range metas {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString(fmt.Sprintf("- /%s: %s\n", meta.Name, desc))
	}
	return sb.String()
}

// ----- SkillForkHost implementation on *Model -----

// ActivateSkill delegates to the underlying Agent so RunInline can pin the
// SOP to env context. Safe to call before m.ag exists (no-op).

func (m Model) ActivateSkill(name, body string) {
	if m.ag != nil {
		m.ag.ActivateSkill(name, body)
	}
}

// SetToolFilter installs a tool visibility filter on the Agent (used by
// Teams coordinator mode). Passing nil clears the filter.

func (m Model) SetToolFilter(allow func(name string) bool) {
	if m.ag == nil {
		return
	}
	m.ag.SetToolFilter(allow)
}

// ToolRegistry exposes the live registry for fail-fast checks and
// directory-type tool registration.

func (m Model) ToolRegistry() *tools.Registry {
	return m.registry
}

// SnapshotParentMessages copies the current main-conversation message log so
// the fork executor can seed the sub-agent per fork_context. Returns a
// shallow copy; callers must not mutate the slice.

func (m Model) SnapshotParentMessages() []conversation.Message {
	if m.conversation == nil {
		return nil
	}
	src := m.conversation.GetMessages()
	out := make([]conversation.Message, len(src))
	copy(out, src)
	return out
}

// RunSubAgent runs `body` as the first user message in an isolated
// sub-agent. The sub-agent gets a filtered registry honoring allowedTools
// (system tools always pass) and the same LLM client / protocol as the
// main loop. Blocks until the sub-agent reaches LoopComplete or errors;
// returns the final assistant text.
//
// Caller is expected to dispatch this on a goroutine (via tea.Cmd) — the
// channel drain here is synchronous and will freeze the UI if invoked on
// the bubbletea Update path.

func (m Model) RunSubAgent(ctx context.Context, body string, seed []conversation.Message, _ string) (string, error) {
	if m.client == nil {
		return "", fmt.Errorf("RunSubAgent: no llm client (provider not selected)")
	}
	subConv := conversation.NewManager()
	for _, msg := range seed {
		switch msg.Role {
		case "user":
			subConv.AddUserMessage(msg.Content)
		case "assistant":
			subConv.AddAssistantMessage(msg.Content)
		}
	}
	subConv.AddUserMessage(body)

	subAgent := agent.New(m.client, m.registry, "")
	if m.selectedProvider != nil {
		subAgent.Protocol = m.selectedProvider.Protocol
		subAgent.ContextWindow = m.selectedProvider.GetContextWindow()
		subAgent.MaxOutputTokens = m.selectedProvider.GetMaxOutputTokens()
	}
	subAgent.MaxIterations = 50

	var output strings.Builder
	ch := subAgent.Run(ctx, subConv)
	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			output.WriteString(e.Text)
		case agent.ErrorEvent:
			return output.String(), fmt.Errorf("%s", e.Message)
		}
	}
	return output.String(), nil
}

func (m *Model) loadCustomInstructions(wd string) string {
	return memory.LoadInstructions(wd)
}

// installMemoryExtractor wires ch09 background memory extraction onto the
// given agent. Constructs an Extractor with the current TUI context and
// hooks it onto ag.OnLoopComplete. Returns the Extractor so the caller
// can store it on Model.memoryExtractor for later Drain.

func (m *Model) installMemoryExtractor(ag *agent.Agent, wd, protocol string) *extractor.Extractor {
	if m.client == nil || m.conversation == nil {
		return nil
	}
	conv := m.conversation
	extr := extractor.InitExtractMemories(extractor.Deps{
		MemoryDir:     memory.GetAutoMemPath(wd),
		UserMemoryDir: memory.GetUserAutoMemPath(),
		ProjectRoot:   wd,
		Client:        m.client,
		ToolRegistry:  m.registry,
		Protocol:      protocol,
		Conversation:  conv,
		AppendSystem:  func(s string) { conv.AddSystemReminder(s) },
	})

	// 记忆整理器：后台自动合并重复、删除过时、修正矛盾
	consolidator := consolidation.NewConsolidator(consolidation.Deps{
		MemoryDir:     memory.GetAutoMemPath(wd),
		UserMemoryDir: memory.GetUserAutoMemPath(),
		ProjectRoot:   wd,
		Client:        m.client,
		ToolRegistry:  m.registry,
		Protocol:      protocol,
		Conversation:  conv,
		AppendSystem:  func(s string) { conv.AddSystemReminder(s) },
	})
	m.memoryConsolidator = consolidator

	ag.OnLoopComplete = func(_ *conversation.Manager) {
		_ = extr.Execute(context.Background())
		consolidator.MaybeRun(context.Background())
	}
	return extr
}

// prefetchRelevantMemories runs the recall selector in a goroutine and
// returns a channel that will receive the rendered system-reminder
// string (or "" if nothing was selected / selector timed out). Caller
// must read from the channel exactly once with its own timeout.
//
// Fires a fresh side-query llm.Client per call so the selector's
// SYSTEM prompt is independent of the main conversation's system prompt.

func (m *Model) prefetchRelevantMemories(query string) <-chan string {
	out := make(chan string, 1)
	if m.memoryMgr == nil || m.selectedProvider == nil {
		out <- ""
		return out
	}
	provider := m.selectedProvider
	memDir := m.memoryMgr.Dir()
	userMemDir := m.memoryMgr.UserDir()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		selector := func(ctx context.Context, sys, user string) (string, error) {
			sideClient, err := llm.NewClient(provider, sys)
			if err != nil {
				return "", err
			}
			miniConv := conversation.NewManager()
			miniConv.AddUserMessage(user)
			events, errs := sideClient.Stream(ctx, miniConv, nil)
			var sb strings.Builder
			for ev := range events {
				if td, ok := ev.(llm.TextDelta); ok {
					sb.WriteString(td.Text)
				}
			}
			select {
			case err := <-errs:
				if err != nil {
					return "", err
				}
			default:
			}
			return sb.String(), nil
		}
		results, _ := memory.FindRelevantMemories(ctx, query, userMemDir, memDir, nil, nil, selector)
		out <- renderRelevantMemoriesReminder(results)
	}()
	return out
}

// collectPrefetchedRecall waits up to timeout for the prefetch channel
// to produce a rendered reminder, then injects it as a system-reminder
// on the given conversation. If the timeout fires first, the prefetch
// goroutine keeps running but its result is dropped — recall is
// best-effort and must not stall the user's main request.

func collectPrefetchedRecall(conv *conversation.Manager, prefetchCh <-chan string, timeout time.Duration) {
	if conv == nil || prefetchCh == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reminder := <-prefetchCh:
		if reminder != "" {
			conv.AddSystemReminder(reminder)
		}
	case <-timer.C:
		// give up — selector still runs in background but result is discarded
	}
}

// renderRelevantMemoriesReminder formats up to 5 recalled memory files
// as a single system-reminder body. Each memory gets a freshness header
// (today / N days ago) and its file content inline. Files that fail to
// read are silently skipped.

func renderRelevantMemoriesReminder(memories []memory.RelevantMemory) string {
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("The following relevant memories from prior conversations may help:\n\n")
	for _, mem := range memories {
		data, err := os.ReadFile(mem.Path)
		if err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("## Memory: %s (saved %s)\n\n", filepath.Base(mem.Path), memory.MemoryAge(mem.MtimeMs)))
		if note := memory.MemoryFreshnessText(mem.MtimeMs); note != "" {
			sb.WriteString(note)
			sb.WriteString("\n\n")
		}
		sb.Write(data)
		sb.WriteString("\n\n---\n\n")
	}
	return sb.String()
}
