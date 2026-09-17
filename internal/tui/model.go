package tui

import (
	"context"
	"time"

	"github.com/ericthz/zebra-code/internal/agent"
	"github.com/ericthz/zebra-code/internal/agents"
	"github.com/ericthz/zebra-code/internal/commands"
	"github.com/ericthz/zebra-code/internal/config"
	"github.com/ericthz/zebra-code/internal/conversation"
	"github.com/ericthz/zebra-code/internal/filehistory"
	"github.com/ericthz/zebra-code/internal/hooks"
	"github.com/ericthz/zebra-code/internal/llm"
	"github.com/ericthz/zebra-code/internal/mcp"
	"github.com/ericthz/zebra-code/internal/memory"
	"github.com/ericthz/zebra-code/internal/memory/consolidation"
	"github.com/ericthz/zebra-code/internal/memory/extractor"
	"github.com/ericthz/zebra-code/internal/permissions"
	"github.com/ericthz/zebra-code/internal/session"
	"github.com/ericthz/zebra-code/internal/skills"
	"github.com/ericthz/zebra-code/internal/teams"
	"github.com/ericthz/zebra-code/internal/todo"
	"github.com/ericthz/zebra-code/internal/tools"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
)

type appState int

const (
	stateProviderSelect appState = iota
	stateChat
	stateResume
)

type chatMessage struct {
	role          string
	content       string
	toolGroup     []toolBlockInfo
	subAgentBlock *subAgentBlock
	expanded      bool
}

type subAgentBlock struct {
	desc      string
	agentType string
	toolUses  []toolBlockInfo
	done      bool
	toolCount int
	totalTime float64
}

type toolBlockInfo struct {
	toolName  string
	args      map[string]any
	output    string
	isError   bool
	elapsed   float64
	collapsed bool
	loading   bool
}

type agentEventMsg struct {
	event agent.AgentEvent
	ch    <-chan agent.AgentEvent
}

type agentDoneMsg struct {
	ch <-chan agent.AgentEvent
}

type agentErrMsg struct{ err error }

type agentReadyMsg struct {
	ch <-chan agent.AgentEvent
}

type mailboxPollMsg struct{}

type mcpReadyMsg struct{ result mcp.ConnectResult }

type compactDoneMsg struct {
	message string
	err     error
}

// forkSkillDoneMsg is dispatched when a fork-mode Skill's sub-agent has
// reached LoopComplete (or failed). Update injects result as a single
// assistant chatMessage into the main conversation log so the user sees
// the sub-agent's final answer without it polluting the parent agent's
// context window.

type forkSkillDoneMsg struct {
	name   string
	result string
	err    error
}

type Model struct {
	providers        []config.ProviderConfig
	selectedProvider *config.ProviderConfig
	client           llm.Client
	registry         *tools.Registry
	ag               *agent.Agent

	state     appState
	streaming bool

	textarea textarea.Model
	viewport viewport.Model
	width    int
	height   int
	ready    bool

	providerCursor int

	conversation *conversation.Manager

	chatMessages []chatMessage
	toolBlocks   []toolBlockInfo
	streamBuf    string
	agentCh      <-chan agent.AgentEvent
	cancelStream context.CancelFunc

	totalInput  int
	totalOutput int

	permDialog   bool
	permToolName string
	permDesc     string
	permRespCh   chan<- agent.PermissionResponse
	permCursor   int

	cmdRegistry   *commands.Registry
	slashMenuOpen bool
	slashMatches  []*commands.Command
	slashCursor   int

	userScrolled  bool
	committedUpTo int
	bannerPrinted bool

	atMenuOpen bool
	atMatches  []string
	atCursor   int
	atPrefix   string

	spinner       spinner.Model
	thinking      bool
	thinkingStart time.Time
	thinkingDone  float64
	thinkingVerb  string

	instructionsContent string
	memoryContent       string

	mcpConfigs        []config.MCPServerConfig
	mcpMgr            *mcp.Manager
	mcpConnecting     bool
	mcpInstructions   string
	mcpInstructionsOK bool
	mcpServerInfo     string
	hookConfigs       []hooks.Hook

	historyEntries []string
	historyIndex   int
	historyDraft   string

	sessionID    string
	fileHistory  *filehistory.History
	defaultTools tools.DefaultTools
	prePlanMode  permissions.PermissionMode

	planApprovalDialog bool
	planApprovalCursor int
	planApprovalInput  string

	rewindDialog       bool
	rewindSnapshots    []filehistory.Snapshot
	rewindCursor       int
	rewindPhase        int // 0=select checkpoint, 1=select restore option
	rewindOptionCursor int

	askUserCh          chan tools.AskUserRequest
	subAgentProgressCh chan agents.SubAgentProgress
	activeSubAgent     *subAgentBlock
	askUserDialog      bool
	askUserQuestions   []tools.Question
	askUserCursors     []int
	askUserSelected    []map[int]bool
	askUserOther       []string
	askUserQIdx        int
	askUserRespCh      chan tools.QuestionResponse
	askUserAnswered    map[int]string
	askUserOnSubmit    bool
	askUserSubmitIdx   int
	skillCatalog       *skills.Catalog
	taskMgr            *agents.TaskManager
	todoList           *todo.TaskList
	memoryMgr          *memory.Manager
	memoryExtractor    *extractor.Extractor
	memoryConsolidator *consolidation.Consolidator
	teamMgr            *teams.TeamManager

	sandboxDialog         bool                 // 沙箱模式选择对话框是否打开
	sandboxCursor         int                  // 当前选中的沙箱模式索引
	sandboxCfg            config.SandboxConfig // 配置文件中的沙箱设置
	EnableCoordinatorMode bool                 // Coordinator 模式配置开关
	ForkDisabled          bool                 // 关掉 fork 后，省略 subagent_type 回退到通用 agent

	resumeSessions  []session.SessionInfo
	resumeFiltered  []session.SessionInfo
	resumeCursor    int
	resumeSearch    string
	resumeScrollTop int

	hasExitedPlanMode bool // 记录本次会话是否曾退出过 Plan Mode，用于重入时注入提示
}
