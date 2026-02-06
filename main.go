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
	showPreview = flag.Bool("preview", true, "Show pane preview on right side")
	listOnly    = flag.Bool("list", false, "List agents and exit (no TUI)")
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
	Session   string      // tmux session name
	Window    int         // tmux window index
	Pane      int         // tmux pane index
	Status    AgentStatus
	LastLine  string      // Last line of output (for status detection)
	UpdatedAt time.Time
}

// Model is the Bubble Tea model
type Model struct {
	agents      []Agent
	cursor      int
	width       int
	height      int
	showPreview bool   // Show pane preview on right side
	preview     string // Cached preview content
	err         error
	quitting    bool
}

// Messages
type tickMsg time.Time
type agentUpdateMsg []Agent
type previewUpdateMsg string
type attachResultMsg struct {
	output string
	err    error
}

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("99")).
			MarginBottom(1)

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212")).
			Background(lipgloss.Color("236"))

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	statusRunning = lipgloss.NewStyle().
			Foreground(lipgloss.Color("82")) // Green

	statusWaiting = lipgloss.NewStyle().
			Foreground(lipgloss.Color("220")) // Yellow

	statusIdle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")) // Gray

	statusError = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")) // Red

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginTop(1)

	paneStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(0, 1)
)

func initialModel() Model {
	return Model{
		agents:      []Agent{},
		cursor:      0,
		showPreview: *showPreview,
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

	// Get all panes across all sessions in one call
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
	// These need user action to proceed
	permissionPatterns := []string{
		"Do you want to proceed?",
		"Do you want to make this edit",
		"Yes, and don't ask",
		"Esc to cancel",
		"❯ 1.",  // Selection menu with cursor
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
	// Look for the Claude Code input prompt pattern
	promptPatterns := []string{
		"⏵⏵",          // Double play button = ready for input
		"────────────", // Horizontal line separator before prompt
	}
	for _, pattern := range promptPatterns {
		if strings.Contains(recentContent, pattern) {
			// Check if this is actually a prompt area (not mid-output)
			if strings.Contains(lastLine, "⏵") || strings.Contains(lastLine, "accept edits") {
				return StatusIdle, truncate(lastLine, 50)
			}
		}
	}

	// Check for recent activity by looking for tool indicators
	toolPattern := regexp.MustCompile(`●\s*(Bash|Read|Write|Edit|Glob|Grep|Task)`)
	if toolPattern.MatchString(content) {
		// Could be running or just completed
		if strings.Contains(lastLine, "⎿") && !strings.Contains(lastLine, "Running") {
			return StatusIdle, truncate(lastLine, 50)
		}
	}

	// Default to idle
	return StatusIdle, truncate(lastLine, 50)
}

func truncate(s string, maxLen int) string {
	// Strip ANSI codes for length calculation
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	clean := ansiRegex.ReplaceAllString(s, "")

	if len(clean) <= maxLen {
		return clean
	}
	return clean[:maxLen-3] + "..."
}

// fetchPreview gets the pane content for preview
func fetchPreview(target string, height int) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
		output, err := cmd.Output()
		if err != nil {
			return previewUpdateMsg(fmt.Sprintf("Error: %v", err))
		}

		lines := strings.Split(string(output), "\n")
		// Get last N lines that fit in preview
		previewLines := height - 4 // Account for borders and padding
		if previewLines < 5 {
			previewLines = 5
		}
		if len(lines) > previewLines {
			lines = lines[len(lines)-previewLines:]
		}

		return previewUpdateMsg(strings.Join(lines, "\n"))
	}
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
				return m, m.updatePreviewCmd()
			}

		case key.Matches(msg, keys.Down):
			if m.cursor < len(m.agents)-1 {
				m.cursor++
				return m, m.updatePreviewCmd()
			}

		case key.Matches(msg, keys.Attach):
			if len(m.agents) > 0 && m.cursor < len(m.agents) {
				agent := m.agents[m.cursor]
				target := fmt.Sprintf("%s:%d.%d", agent.Session, agent.Window, agent.Pane)
				// Attach to tmux session in a new terminal
				return m, attachToAgent(target)
			}

		case key.Matches(msg, keys.Refresh):
			return m, detectAgents

		case key.Matches(msg, keys.TogglePreview):
			m.showPreview = !m.showPreview
			if m.showPreview {
				return m, m.updatePreviewCmd()
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.showPreview {
			return m, m.updatePreviewCmd()
		}

	case tickMsg:
		cmds := []tea.Cmd{tickCmd(), detectAgents}
		if m.showPreview && len(m.agents) > 0 {
			cmds = append(cmds, m.updatePreviewCmd())
		}
		return m, tea.Batch(cmds...)

	case agentUpdateMsg:
		m.agents = []Agent(msg)
		// Keep cursor in bounds
		if m.cursor >= len(m.agents) && len(m.agents) > 0 {
			m.cursor = len(m.agents) - 1
		}
		// Update preview for current selection
		if m.showPreview && len(m.agents) > 0 {
			return m, m.updatePreviewCmd()
		}

	case previewUpdateMsg:
		m.preview = string(msg)

	case attachResultMsg:
		if msg.err != nil {
			m.err = msg.err
		}
	}

	return m, nil
}

func (m Model) updatePreviewCmd() tea.Cmd {
	if len(m.agents) == 0 || m.cursor >= len(m.agents) {
		return nil
	}
	agent := m.agents[m.cursor]
	target := fmt.Sprintf("%s:%d.%d", agent.Session, agent.Window, agent.Pane)
	return fetchPreview(target, m.height)
}

func attachToAgent(target string) tea.Cmd {
	return func() tea.Msg {
		// Use tmux switch-client if we're in tmux, otherwise attach
		if os.Getenv("TMUX") != "" {
			// Extract session name from target
			parts := strings.Split(target, ":")
			if len(parts) > 0 {
				cmd := exec.Command("tmux", "switch-client", "-t", target)
				err := cmd.Run()
				return attachResultMsg{err: err}
			}
		} else {
			// Not in tmux, attach directly
			cmd := exec.Command("tmux", "attach-session", "-t", target)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			return attachResultMsg{err: err}
		}
		return attachResultMsg{}
	}
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	// Calculate layout dimensions
	listWidth := 35
	if !m.showPreview {
		listWidth = m.width
	}
	previewWidth := m.width - listWidth - 3 // Account for separator

	// Build agent list
	var listBuilder strings.Builder
	listBuilder.WriteString(titleStyle.Render("Agent Monitor"))
	listBuilder.WriteString("\n\n")

	if len(m.agents) == 0 {
		listBuilder.WriteString(normalStyle.Render("No agents found.\n"))
		listBuilder.WriteString(normalStyle.Render("Start claude in tmux.\n"))
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

			// Compact format: ● session-name (status)
			line := fmt.Sprintf("%s %s", symbol, agent.Name)

			// Truncate to fit list width
			maxLineLen := listWidth - 4
			if len(line) > maxLineLen {
				line = line[:maxLineLen-3] + "..."
			}

			if i == m.cursor {
				listBuilder.WriteString(selectedStyle.Render("> "+line) + "\n")
			} else {
				listBuilder.WriteString(normalStyle.Render("  "+line) + "\n")
			}
		}
	}

	// Help line
	var help string
	if m.showPreview {
		help = "j/k:nav enter:attach p:preview q:quit"
	} else {
		help = "↑/↓:nav enter:attach p:preview r:refresh q:quit"
	}
	listBuilder.WriteString(helpStyle.Render(help))

	if m.err != nil {
		listBuilder.WriteString(fmt.Sprintf("\n%s", statusError.Render(m.err.Error())))
	}

	listContent := listBuilder.String()

	// If no preview, just return the list
	if !m.showPreview || previewWidth < 20 {
		return listContent
	}

	// Build preview pane
	var previewTitle string
	if len(m.agents) > 0 && m.cursor < len(m.agents) {
		previewTitle = m.agents[m.cursor].Name
	} else {
		previewTitle = "Preview"
	}

	previewContent := m.preview
	if previewContent == "" {
		previewContent = "No preview available"
	}

	// Wrap preview in a styled box
	previewBox := paneStyle.
		Width(previewWidth - 2).
		Height(m.height - 4).
		Render(previewContent)

	previewHeader := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("99")).
		Render(previewTitle)

	previewPane := previewHeader + "\n" + previewBox

	// Join list and preview horizontally
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		listContent,
		"  ",
		previewPane,
	)
}

// Key bindings
type keyMap struct {
	Up            key.Binding
	Down          key.Binding
	Attach        key.Binding
	Refresh       key.Binding
	TogglePreview key.Binding
	Quit          key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	Attach: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "attach"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "refresh"),
	),
	TogglePreview: key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "toggle preview"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
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
}

// detectAgentsSync is a synchronous version for --list mode
func detectAgentsSync() []Agent {
	msg := detectAgents()
	if agents, ok := msg.(agentUpdateMsg); ok {
		return []Agent(agents)
	}
	return nil
}
