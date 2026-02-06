package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	listOnly    = flag.Bool("list", false, "List agents and exit (no TUI)")
	outerSocket = flag.String("socket", "agent-monitor", "Outer tmux socket name for pane control")
	noAttach    = flag.Bool("no-attach", false, "Don't attach to agents on Enter (just list)")
)

// Agent status
type AgentStatus int

const (
	StatusUnknown AgentStatus = iota
	StatusRunning             // Agent is actively working (streaming output)
	StatusWaiting             // Waiting for user input
	StatusIdle                // Idle/ready for input
	StatusError               // Error state
)

func (s AgentStatus) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusWaiting:
		return "waiting"
	case StatusIdle:
		return "idle"
	case StatusError:
		return "error"
	default:
		return "unknown"
	}
}

func (s AgentStatus) Symbol() string {
	switch s {
	case StatusRunning:
		return "●" // Green dot
	case StatusWaiting:
		return "◐" // Half circle (needs input)
	case StatusIdle:
		return "○" // Empty circle
	case StatusError:
		return "✕" // X mark
	default:
		return "?"
	}
}

// Agent represents a tracked Claude Code agent
type Agent struct {
	Name      string
	Session   string // tmux session name
	Window    int    // tmux window index
	Pane      int    // tmux pane index
	Status    AgentStatus
	LastLine  string // Last line of output (for status detection)
	UpdatedAt time.Time
}

func (a Agent) Target() string {
	return fmt.Sprintf("%s:%d.%d", a.Session, a.Window, a.Pane)
}

// Model is the Bubble Tea model
type Model struct {
	agents      []Agent
	cursor      int
	width       int
	height      int
	outerSocket string // Socket for outer tmux (to control right pane)
	attached    string // Currently attached agent target
	err         error
	quitting    bool
}

// Messages
type tickMsg time.Time
type agentUpdateMsg []Agent
type attachResultMsg struct {
	target string
	err    error
}

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("99"))

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212")).
			Background(lipgloss.Color("236"))

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	statusRunning = lipgloss.NewStyle().
			Foreground(lipgloss.Color("82")) // Green

	statusWaiting = lipgloss.NewStyle().
			Foreground(lipgloss.Color("220")) // Yellow

	statusIdle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")) // Gray

	statusError = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")) // Red

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	attachedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("82")).
			Bold(true)
)

func initialModel() Model {
	return Model{
		agents:      []Agent{},
		cursor:      0,
		outerSocket: *outerSocket,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(),
		detectAgents,
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// detectAgents scans tmux sessions for Claude Code instances
func detectAgents() tea.Msg {
	agents := []Agent{}

	// Get all panes across all sessions in one call (default socket only)
	cmd := exec.Command("tmux", "list-panes", "-a",
		"-F", "#{session_name}:#{window_index}.#{pane_index} #{pane_current_command}")
	output, err := cmd.Output()
	if err != nil {
		return agentUpdateMsg(agents)
	}

	seen := make(map[string]bool)

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Only look for claude processes
		if !strings.Contains(line, "claude") {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 1 {
			continue
		}

		target := parts[0]

		// Avoid duplicates
		if seen[target] {
			continue
		}
		seen[target] = true

		// Parse session:window.pane
		colonIdx := strings.Index(target, ":")
		if colonIdx == -1 {
			continue
		}

		session := target[:colonIdx]
		rest := target[colonIdx+1:]

		var window, pane int
		fmt.Sscanf(rest, "%d.%d", &window, &pane)

		agent := Agent{
			Name:      session,
			Session:   session,
			Window:    window,
			Pane:      pane,
			Status:    StatusUnknown,
			UpdatedAt: time.Now(),
		}

		// Detect status by capturing pane content
		agent.Status, agent.LastLine = detectAgentStatus(target)

		agents = append(agents, agent)
	}

	// Sort by name
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Name < agents[j].Name
	})

	return agentUpdateMsg(agents)
}

// detectAgentStatus captures pane content and determines agent state
func detectAgentStatus(target string) (AgentStatus, string) {
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
	output, err := cmd.Output()
	if err != nil {
		return StatusError, ""
	}

	lines := strings.Split(string(output), "\n")

	// Find last few non-empty lines for context
	var lastLines []string
	for i := len(lines) - 1; i >= 0 && len(lastLines) < 10; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			lastLines = append([]string{line}, lastLines...)
		}
	}

	var lastLine string
	if len(lastLines) > 0 {
		lastLine = lastLines[len(lastLines)-1]
	}

	// Check last 10 lines for patterns (more accurate than whole buffer)
	recentContent := strings.Join(lastLines, "\n")
	content := string(output)

	// Running: streaming output indicators (check recent content first)
	runningPatterns := []string{
		"Running…",
		"⎿  Running",
		"Thinking…",
		"Waiting…",
	}
	for _, pattern := range runningPatterns {
		if strings.Contains(recentContent, pattern) {
			return StatusRunning, truncate(lastLine, 50)
		}
	}

	// Waiting for PERMISSION: shows selection prompt for approval
	permissionPatterns := []string{
		"Do you want to proceed?",
		"Do you want to make this edit",
		"Yes, and don't ask",
		"Esc to cancel",
		"❯ 1.",
		"❯ 2.",
		"[y/N]",
		"(y/n)",
		"Press Enter",
		"Tab to amend",
	}
	for _, pattern := range permissionPatterns {
		if strings.Contains(recentContent, pattern) {
			return StatusWaiting, truncate(lastLine, 50)
		}
	}

	// Idle at prompt: ready for new command input
	promptPatterns := []string{
		"⏵⏵",
		"────────────",
	}
	for _, pattern := range promptPatterns {
		if strings.Contains(recentContent, pattern) {
			if strings.Contains(lastLine, "⏵") || strings.Contains(lastLine, "accept edits") {
				return StatusIdle, truncate(lastLine, 50)
			}
		}
	}

	// Check for recent activity by looking for tool indicators
	toolPattern := regexp.MustCompile(`●\s*(Bash|Read|Write|Edit|Glob|Grep|Task)`)
	if toolPattern.MatchString(content) {
		if strings.Contains(lastLine, "⎿") && !strings.Contains(lastLine, "Running") {
			return StatusIdle, truncate(lastLine, 50)
		}
	}

	return StatusIdle, truncate(lastLine, 50)
}

func truncate(s string, maxLen int) string {
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	clean := ansiRegex.ReplaceAllString(s, "")

	if len(clean) <= maxLen {
		return clean
	}
	return clean[:maxLen-3] + "..."
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			m.quitting = true
			return m, tea.Quit

		case key.Matches(msg, keys.Up):
			if m.cursor > 0 {
				m.cursor--
			}

		case key.Matches(msg, keys.Down):
			if m.cursor < len(m.agents)-1 {
				m.cursor++
			}

		case key.Matches(msg, keys.Attach):
			if len(m.agents) > 0 && m.cursor < len(m.agents) && !*noAttach {
				agent := m.agents[m.cursor]
				return m, m.attachToAgent(agent)
			}

		case key.Matches(msg, keys.FocusRight):
			// Focus the right pane (where agent is attached)
			return m, m.focusRightPane()

		case key.Matches(msg, keys.Refresh):
			return m, detectAgents
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(tickCmd(), detectAgents)

	case agentUpdateMsg:
		m.agents = []Agent(msg)
		if m.cursor >= len(m.agents) && len(m.agents) > 0 {
			m.cursor = len(m.agents) - 1
		}

	case attachResultMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.attached = msg.target
			m.err = nil
		}
	}

	return m, nil
}

// attachToAgent updates the right pane to show the selected agent
func (m Model) attachToAgent(agent Agent) tea.Cmd {
	return func() tea.Msg {
		target := agent.Target()

		// First, detach any currently attached tmux session in the right pane
		// by sending the detach command (C-b d for default prefix)
		// This returns us to the shell
		exec.Command("tmux", "-L", m.outerSocket, "send-keys", "-t", "0.1", "C-b", "d").Run()

		// Small delay to let detach complete
		time.Sleep(150 * time.Millisecond)

		// Kill any other process (like the placeholder)
		exec.Command("tmux", "-L", m.outerSocket, "send-keys", "-t", "0.1", "C-c").Run()
		time.Sleep(50 * time.Millisecond)

		// Send the attach command
		// unset TMUX so nested attach works
		attachCmd := fmt.Sprintf("unset TMUX; tmux attach-session -t '%s'", target)
		exec.Command("tmux", "-L", m.outerSocket, "send-keys", "-t", "0.1", attachCmd, "Enter").Run()

		return attachResultMsg{target: target, err: nil}
	}
}

// focusRightPane switches focus to the right pane
func (m Model) focusRightPane() tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("tmux", "-L", m.outerSocket, "select-pane", "-t", "0.1")
		cmd.Run()
		return nil
	}
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("Agent Monitor"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d agents", len(m.agents))))
	b.WriteString("\n\n")

	if len(m.agents) == 0 {
		b.WriteString(normalStyle.Render("No agents found.\n"))
		b.WriteString(dimStyle.Render("Start claude in tmux.\n"))
	} else {
		for i, agent := range m.agents {
			// Status symbol with color
			var symbol string
			switch agent.Status {
			case StatusRunning:
				symbol = statusRunning.Render(agent.Status.Symbol())
			case StatusWaiting:
				symbol = statusWaiting.Render(agent.Status.Symbol())
			case StatusIdle:
				symbol = statusIdle.Render(agent.Status.Symbol())
			case StatusError:
				symbol = statusError.Render(agent.Status.Symbol())
			default:
				symbol = agent.Status.Symbol()
			}

			// Show attached indicator
			name := agent.Name
			if agent.Target() == m.attached {
				name = attachedStyle.Render(name + " ◀")
			}

			line := fmt.Sprintf("%s %s", symbol, name)

			if i == m.cursor {
				b.WriteString(selectedStyle.Render("> " + line))
			} else {
				b.WriteString(normalStyle.Render("  " + line))
			}
			b.WriteString("\n")
		}
	}

	// Status line
	b.WriteString("\n")
	if m.err != nil {
		b.WriteString(statusError.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
	}

	// Help
	b.WriteString(helpStyle.Render("j/k:nav  ⏎:attach  l:focus  r:refresh  q:quit"))

	return b.String()
}

// Key bindings
type keyMap struct {
	Up         key.Binding
	Down       key.Binding
	Attach     key.Binding
	FocusRight key.Binding
	Refresh    key.Binding
	Quit       key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
	),
	Attach: key.NewBinding(
		key.WithKeys("enter"),
	),
	FocusRight: key.NewBinding(
		key.WithKeys("l", "right"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("r"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
	),
}

func main() {
	flag.Parse()

	// List mode: just print agents and exit
	if *listOnly {
		agents := detectAgentsSync()
		if len(agents) == 0 {
			fmt.Println("No Claude Code agents detected.")
			return
		}
		for _, agent := range agents {
			symbol := agent.Status.Symbol()
			fmt.Printf("%s %s (%s)\n", symbol, agent.Name, agent.Status)
		}
		return
	}

	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Kill the outer tmux session when we quit
	if *outerSocket != "" {
		exec.Command("tmux", "-L", *outerSocket, "kill-session").Run()
	}
}

func detectAgentsSync() []Agent {
	msg := detectAgents()
	if agents, ok := msg.(agentUpdateMsg); ok {
		return []Agent(agents)
	}
	return nil
}
