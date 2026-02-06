package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"
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

// Config types for group configuration
type GroupConfig struct {
	Name     string   `yaml:"name"`
	Sessions []string `yaml:"sessions"`
}

type Config struct {
	Groups []GroupConfig `yaml:"groups"`
}

// Group holds a named group of agents for display
type Group struct {
	Name   string
	Agents []Agent
}

// loadConfig reads the groups config from ~/.config/agent-monitor/groups.yaml
func loadConfig() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}
	}
	path := filepath.Join(home, ".config", "agent-monitor", "groups.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}
	}
	return cfg
}

// Spinner frames for running agents
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Model is the Bubble Tea model
type Model struct {
	agents       []Agent
	cursor       int
	width        int
	height       int
	outerSocket  string // Socket for outer tmux (to control right pane)
	attached     string // Currently attached agent target
	err          error
	quitting     bool
	config       Config   // Loaded group config
	groups       []Group  // Computed groups for display
	flatAgents   []Agent  // Flattened agent list in display order (cursor indexes into this)
	spinnerFrame int      // Animation frame counter
}

// Messages
type tickMsg time.Time
type agentUpdateMsg []Agent
type spinnerTickMsg time.Time
type attachResultMsg struct {
	target string
	err    error
}

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#cc66ff")).
			Background(lipgloss.Color("#1a0033")).
			Padding(0, 1)

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

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#9933ff")).
			Padding(0, 1)

	helpPanelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#4a0080")).
			Padding(0, 1)

	// Cycling group header colors (purple/violet shades)
	groupHeaderColors = []lipgloss.Color{
		lipgloss.Color("#cc66ff"),
		lipgloss.Color("#b84dff"),
		lipgloss.Color("#a333ff"),
		lipgloss.Color("#9933ff"),
		lipgloss.Color("#8c1aff"),
	}

	// "Other" group gets a dimmer blue-gray
	otherGroupColor = lipgloss.Color("#6677aa")
)

func initialModel() Model {
	return Model{
		agents:      []Agent{},
		cursor:      0,
		outerSocket: *outerSocket,
		config:      loadConfig(),
	}
}

// buildGroups maps agents into config-defined groups and computes flatAgents.
func (m *Model) buildGroups() {
	if len(m.config.Groups) == 0 {
		// No config — flat list, no headers
		m.groups = nil
		m.flatAgents = m.agents
		return
	}

	// Build a lookup: session name -> config group index
	sessionToGroup := make(map[string]int)
	for i, gc := range m.config.Groups {
		for _, s := range gc.Sessions {
			sessionToGroup[s] = i
		}
	}

	// Bucket agents into groups
	buckets := make([][]Agent, len(m.config.Groups))
	var other []Agent
	for _, agent := range m.agents {
		if idx, ok := sessionToGroup[agent.Session]; ok {
			buckets[idx] = append(buckets[idx], agent)
		} else {
			other = append(other, agent)
		}
	}

	// Build groups preserving config order, skip empty groups
	m.groups = nil
	m.flatAgents = nil
	for i, gc := range m.config.Groups {
		if len(buckets[i]) == 0 {
			continue
		}
		g := Group{Name: gc.Name, Agents: buckets[i]}
		m.groups = append(m.groups, g)
		m.flatAgents = append(m.flatAgents, buckets[i]...)
	}
	if len(other) > 0 {
		g := Group{Name: "Other", Agents: other}
		m.groups = append(m.groups, g)
		m.flatAgents = append(m.flatAgents, other...)
	}
}

// spinnerTickCmd sends a tick every 100ms for spinner animation.
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg(t)
	})
}

// hasRunningAgents checks if any agent has StatusRunning.
func (m Model) hasRunningAgents() bool {
	for _, a := range m.agents {
		if a.Status == StatusRunning {
			return true
		}
	}
	return false
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(),
		detectAgents,
		spinnerTickCmd(),
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
			if m.cursor < len(m.flatAgents)-1 {
				m.cursor++
			}

		case key.Matches(msg, keys.Attach):
			if len(m.flatAgents) > 0 && m.cursor < len(m.flatAgents) && !*noAttach {
				agent := m.flatAgents[m.cursor]
				return m, m.attachToAgent(agent)
			}

		case key.Matches(msg, keys.FocusRight):
			return m, m.focusRightPane()

		case key.Matches(msg, keys.Refresh):
			return m, detectAgents
		}

	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			// Account for panel border (1 line) + title + count + blank = 4 lines offset
			// Plus group headers before the clicked position
			clickedY := msg.Y
			idx := m.mouseYToAgentIndex(clickedY)
			if idx >= 0 && idx < len(m.flatAgents) {
				m.cursor = idx
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(tickCmd(), detectAgents)

	case spinnerTickMsg:
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		if m.hasRunningAgents() {
			return m, spinnerTickCmd()
		}
		return m, nil

	case agentUpdateMsg:
		m.agents = []Agent(msg)
		m.buildGroups()
		if m.cursor >= len(m.flatAgents) && len(m.flatAgents) > 0 {
			m.cursor = len(m.flatAgents) - 1
		}
		// Start spinner if there are running agents
		if m.hasRunningAgents() {
			return m, spinnerTickCmd()
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

// mouseYToAgentIndex converts a mouse Y coordinate to a flatAgents index,
// accounting for panel borders, title lines, and group headers.
func (m Model) mouseYToAgentIndex(y int) int {
	// Panel top border = 1, title = 1, count = 1, blank = 1 => content starts at y=4
	line := 4
	agentIdx := 0

	if m.groups != nil {
		for _, g := range m.groups {
			// Group header line
			if y == line {
				return -1 // clicked on header
			}
			line++
			for range g.Agents {
				if y == line {
					return agentIdx
				}
				line++
				agentIdx++
			}
		}
	} else {
		for i := range m.flatAgents {
			if y == line {
				return i
			}
			line++
		}
	}
	return -1
}

// attachToAgent updates the right pane to show the selected agent
func (m Model) attachToAgent(agent Agent) tea.Cmd {
	return func() tea.Msg {
		target := agent.Target()

		// Use respawn-pane to kill whatever is running and start fresh with the attach command
		// This cleanly handles: placeholder, attached tmux session, or anything else
		attachCmd := fmt.Sprintf("unset TMUX; exec tmux attach-session -t '%s'", target)
		exec.Command("tmux", "-L", m.outerSocket, "respawn-pane", "-k", "-t", "0.1", attachCmd).Run()

		// Focus the right pane
		exec.Command("tmux", "-L", m.outerSocket, "select-pane", "-t", "0.1").Run()

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

// renderStatusSymbol returns the colored status symbol for an agent,
// using animated spinner for running agents.
func (m Model) renderStatusSymbol(agent Agent) string {
	switch agent.Status {
	case StatusRunning:
		frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		return statusRunning.Render(frame)
	case StatusWaiting:
		return statusWaiting.Render(agent.Status.Symbol())
	case StatusIdle:
		return statusIdle.Render(agent.Status.Symbol())
	case StatusError:
		return statusError.Render(agent.Status.Symbol())
	default:
		return agent.Status.Symbol()
	}
}

// renderAgentLine renders a single agent line with status symbol and name.
func (m Model) renderAgentLine(agent Agent, idx int) string {
	symbol := m.renderStatusSymbol(agent)

	name := agent.Name
	if agent.Target() == m.attached {
		name = attachedStyle.Render(name + " ◀")
	}

	line := fmt.Sprintf("%s %s", symbol, name)

	if idx == m.cursor {
		return selectedStyle.Render("> " + line)
	}
	return normalStyle.Render("  " + line)
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var content strings.Builder

	// Title
	content.WriteString(titleStyle.Render("Agent Monitor"))
	content.WriteString("\n")
	content.WriteString(dimStyle.Render(fmt.Sprintf("%d agents", len(m.flatAgents))))
	content.WriteString("\n\n")

	if len(m.flatAgents) == 0 {
		content.WriteString(normalStyle.Render("No agents found.\n"))
		content.WriteString(dimStyle.Render("Start claude in tmux.\n"))
	} else if m.groups != nil {
		// Grouped rendering
		agentIdx := 0
		for gi, g := range m.groups {
			// Group header with cycling colors
			var headerColor lipgloss.Color
			if g.Name == "Other" {
				headerColor = otherGroupColor
			} else {
				headerColor = groupHeaderColors[gi%len(groupHeaderColors)]
			}
			headerStyle := lipgloss.NewStyle().
				Bold(true).
				Foreground(headerColor)
			content.WriteString(headerStyle.Render(fmt.Sprintf("┌ %s", g.Name)))
			content.WriteString("\n")

			for _, agent := range g.Agents {
				content.WriteString(m.renderAgentLine(agent, agentIdx))
				content.WriteString("\n")
				agentIdx++
			}
		}
	} else {
		// Flat rendering (no config or single implicit group)
		for i, agent := range m.flatAgents {
			content.WriteString(m.renderAgentLine(agent, i))
			content.WriteString("\n")
		}
	}

	// Error line
	if m.err != nil {
		content.WriteString("\n")
		content.WriteString(statusError.Render(fmt.Sprintf("Error: %v", m.err)))
	}

	// Apply panel border to agent list
	panelWidth := m.width - 2 // account for border
	if panelWidth < 20 {
		panelWidth = 20
	}
	agentPanel := panelStyle.Width(panelWidth).Render(content.String())

	// Help bar with its own border
	helpText := helpStyle.Render("j/k:nav  ⏎:attach  l:focus  r:refresh  q:quit")
	helpPanel := helpPanelStyle.Width(panelWidth).Render(helpText)

	return agentPanel + "\n" + helpPanel
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

	p := tea.NewProgram(initialModel(), tea.WithAltScreen(), tea.WithMouseCellMotion())
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
