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
	StatusUnknown  AgentStatus = iota
	StatusRunning              // Agent is actively working (streaming output)
	StatusPlanning             // Agent is in plan mode (exploring/designing)
	StatusWaiting              // Waiting for user input
	StatusIdle                 // Idle/ready for input
	StatusError                // Error state
)

func (s AgentStatus) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusPlanning:
		return "planning"
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
	case StatusPlanning:
		return "◇" // Diamond (planning)
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

// Planning frames — pulsing diamond (slower cycle via frame division)
var planningFrames = []string{"◇", "◇", "◈", "◆", "◆", "◈"}

// Waiting frames — pulsing half-circle
var waitingFrames = []string{"◐", "◐", "◑", "◑"}

// Gradient colors for the title — subtle purple range
var titleGradient = []lipgloss.Color{
	"#9933ff", "#a64dff", "#b366ff", "#bf80ff",
	"#cc99ff", "#bf80ff", "#b366ff", "#a64dff",
}

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

	statusPlanning = lipgloss.NewStyle().
			Foreground(lipgloss.Color("141")) // Light purple

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
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg(t)
	})
}

// hasAnimatedAgents checks if any agent needs animation (running, planning, or waiting).
func (m Model) hasAnimatedAgents() bool {
	for _, a := range m.agents {
		if a.Status == StatusRunning || a.Status == StatusPlanning || a.Status == StatusWaiting {
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

	// Collect last 20 non-empty lines for pattern matching.
	// Needs to be large enough to see past task checklists that appear below the spinner.
	var lastLines []string
	for i := len(lines) - 1; i >= 0 && len(lastLines) < 20; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			lastLines = append([]string{line}, lastLines...)
		}
	}

	var lastLine string
	if len(lastLines) > 0 {
		lastLine = lastLines[len(lastLines)-1]
	}

	// Find a meaningful activity line (not a prompt, separator, or UI chrome)
	activityLine := findActivityLine(lastLines)

	recentContent := strings.Join(lastLines, "\n")

	// --- Active work detection ---
	// All running patterns are line-start-anchored to avoid false positives
	// from response text that mentions these words mid-paragraph.

	// Active spinner: Claude Code shows activity as "<char> <text>… (<duration> · ...)"
	// The char varies (✻ ✢ ✶ * etc.) but the timing suffix is consistent.
	// Match any line with "…" followed by a parenthesized duration.
	activeSpinner := regexp.MustCompile(`(?m)…\s+\(\d+[ms]`)
	if activeSpinner.MatchString(recentContent) {
		return StatusRunning, truncate(activityLine, 60)
	}

	// Also match spinner lines by prefix char + text + … (without timing, for early display)
	activeSpinnerAlt := regexp.MustCompile(`(?m)^[✻✢✶✦✧✹✺✵✷❋❊⚝*]\s+.+…`)
	if activeSpinnerAlt.MatchString(recentContent) {
		return StatusRunning, truncate(activityLine, 60)
	}

	// Tool execution: ⎿ at line start followed by Running
	if regexp.MustCompile(`(?m)^⎿\s+Running`).MatchString(recentContent) {
		return StatusRunning, truncate(activityLine, 60)
	}

	// Subagent execution: "Running N ... agents…" at line start
	if regexp.MustCompile(`(?m)^●\s+Running\s+\d+`).MatchString(recentContent) {
		return StatusRunning, truncate(activityLine, 60)
	}

	// Streaming indicators at line start (e.g. "* Thinking…" shown during generation)
	if regexp.MustCompile(`(?m)^\*\s+(Thinking|Waiting)…`).MatchString(recentContent) {
		return StatusRunning, truncate(activityLine, 60)
	}

	// --- UI states (checked on wider recent content) ---

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
			return StatusWaiting, truncate(activityLine, 60)
		}
	}

	// Plan mode: agent is exploring/designing
	if strings.Contains(recentContent, "plan mode") {
		return StatusPlanning, truncate(activityLine, 60)
	}

	// Idle at prompt: ready for new command input
	if strings.Contains(lastLine, "⏵⏵") || strings.Contains(lastLine, "accept edits") {
		return StatusIdle, truncate(activityLine, 60)
	}

	return StatusIdle, truncate(activityLine, 60)
}

// findActivityLine scans recent lines bottom-up for a meaningful content line,
// skipping prompts, separators, and UI chrome.
func findActivityLine(lines []string) string {
	skipPatterns := []string{
		"⏵⏵",
		"────",
		"❯",
		"erewhon@",
		"Esc to cancel",
		"Tab to amend",
		"Do you want",
		"accept edits",
		"plan mode",
		"Context left",
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		skip := false
		for _, pat := range skipPatterns {
			if strings.Contains(line, pat) {
				skip = true
				break
			}
		}
		// Skip lines that are just numbers/diff markers
		if !skip && len(line) > 2 {
			return line
		}
	}
	return ""
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
		if m.hasAnimatedAgents() {
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
		if m.hasAnimatedAgents() {
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
	case StatusPlanning:
		frame := planningFrames[m.spinnerFrame%len(planningFrames)]
		return statusPlanning.Render(frame)
	case StatusWaiting:
		frame := waitingFrames[m.spinnerFrame%len(waitingFrames)]
		return statusWaiting.Render(frame)
	case StatusIdle:
		return statusIdle.Render(agent.Status.Symbol())
	case StatusError:
		return statusError.Render(agent.Status.Symbol())
	default:
		return agent.Status.Symbol()
	}
}

// renderAgentLine renders a single agent line with status symbol, name, and last activity.
func (m Model) renderAgentLine(agent Agent, idx int, maxNameLen int) string {
	symbol := m.renderStatusSymbol(agent)

	name := agent.Name
	if agent.Target() == m.attached {
		name = attachedStyle.Render(name + " ◀")
	}

	line := fmt.Sprintf("%s %-*s", symbol, maxNameLen, name)

	// Show truncated last activity in dim text
	if agent.LastLine != "" {
		activity := agent.LastLine
		// Trim available width: 2 (indent) + 2 (symbol+space) + maxNameLen + 2 (gap)
		maxActivity := m.width - 8 - maxNameLen
		if maxActivity > 6 {
			activity = truncate(activity, maxActivity)
			line += "  " + dimStyle.Render(activity)
		}
	}

	if idx == m.cursor {
		return selectedStyle.Render("> " + line)
	}
	return normalStyle.Render("  " + line)
}

// renderGradientTitle renders the title with a subtle shimmer effect.
// The gradient is mostly static with a slow-moving highlight.
func (m Model) renderGradientTitle(text string) string {
	var b strings.Builder
	runes := []rune(text)
	// Shimmer moves one position every 4 animation frames
	shimmerPos := (m.spinnerFrame / 4) % len(runes)
	for i, r := range runes {
		colorIdx := i % len(titleGradient)
		color := titleGradient[colorIdx]
		// Brighten the character near the shimmer position
		dist := i - shimmerPos
		if dist < 0 {
			dist = -dist
		}
		if dist <= 1 {
			color = "#e0b3ff" // bright highlight
		} else if dist == 2 {
			color = "#cc99ff" // softer highlight
		}
		style := lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(color)).
			Background(lipgloss.Color("#1a0033"))
		b.WriteString(style.Render(string(r)))
	}
	return b.String()
}

// renderStatusSummary renders a colored status summary line.
func (m Model) renderStatusSummary() string {
	var running, planning, waiting, idle, errCount int
	for _, a := range m.flatAgents {
		switch a.Status {
		case StatusRunning:
			running++
		case StatusPlanning:
			planning++
		case StatusWaiting:
			waiting++
		case StatusIdle:
			idle++
		case StatusError:
			errCount++
		}
	}

	var parts []string
	if running > 0 {
		parts = append(parts, statusRunning.Render(fmt.Sprintf("%d running", running)))
	}
	if planning > 0 {
		parts = append(parts, statusPlanning.Render(fmt.Sprintf("%d planning", planning)))
	}
	if waiting > 0 {
		parts = append(parts, statusWaiting.Render(fmt.Sprintf("%d waiting", waiting)))
	}
	if idle > 0 {
		parts = append(parts, statusIdle.Render(fmt.Sprintf("%d idle", idle)))
	}
	if errCount > 0 {
		parts = append(parts, statusError.Render(fmt.Sprintf("%d error", errCount)))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, dimStyle.Render(" / "))
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	// Compute max agent name length for alignment
	maxNameLen := 0
	for _, a := range m.flatAgents {
		if len(a.Name) > maxNameLen {
			maxNameLen = len(a.Name)
		}
	}
	// Add space for " ◀" attached indicator
	maxNameLen += 3

	var content strings.Builder

	// Gradient title
	content.WriteString(m.renderGradientTitle(" Agent Monitor "))
	content.WriteString("\n")

	// Status summary or agent count
	if len(m.flatAgents) > 0 {
		content.WriteString(m.renderStatusSummary())
	} else {
		content.WriteString(dimStyle.Render("0 agents"))
	}
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
				content.WriteString(m.renderAgentLine(agent, agentIdx, maxNameLen))
				content.WriteString("\n")
				agentIdx++
			}
		}
	} else {
		// Flat rendering (no config or single implicit group)
		for i, agent := range m.flatAgents {
			content.WriteString(m.renderAgentLine(agent, i, maxNameLen))
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
