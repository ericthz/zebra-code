package tui

import (
	"time"

	"github.com/ericthz/zebra-code/internal/agents"
	"github.com/ericthz/zebra-code/internal/commands"
	"github.com/ericthz/zebra-code/internal/config"
	"github.com/ericthz/zebra-code/internal/conversation"
	"github.com/ericthz/zebra-code/internal/hooks"
	"github.com/ericthz/zebra-code/internal/tools"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func New(providers []config.ProviderConfig, mcpConfigs []config.MCPServerConfig, hookConfigs []hooks.Hook, sandboxCfg ...config.SandboxConfig) Model {
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.SetHeight(1)

	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		Frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		FPS:    80 * time.Millisecond,
	}
	sp.Style = lipgloss.NewStyle().Foreground(brandPurple)

	askCh := make(chan tools.AskUserRequest, 1)
	subProgressCh := make(chan agents.SubAgentProgress, 32)
	dt := tools.CreateDefaultTools()
	reg := dt.Registry
	reg.Register(&tools.AskUserQuestionTool{RequestCh: askCh})

	var sCfg config.SandboxConfig
	if len(sandboxCfg) > 0 {
		sCfg = sandboxCfg[0]
	}

	m := Model{
		providers:          providers,
		mcpConfigs:         mcpConfigs,
		hookConfigs:        hookConfigs,
		sandboxCfg:         sCfg,
		state:              stateProviderSelect,
		textarea:           ta,
		conversation:       conversation.NewManager(),
		registry:           reg,
		defaultTools:       dt,
		cmdRegistry:        commands.CreateDefaultRegistry(),
		spinner:            sp,
		askUserCh:          askCh,
		subAgentProgressCh: subProgressCh,
	}

	if len(providers) == 1 {
		m.state = stateChat
	}

	return m
}

type initSingleProviderMsg struct{}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink}
	if len(m.providers) == 1 {
		cmds = append(cmds, func() tea.Msg { return initSingleProviderMsg{} })
	}
	return tea.Batch(cmds...)
}
