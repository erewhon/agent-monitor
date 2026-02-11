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

// version is set at build time via ldflags
var version = "dev"

var (
	listOnly    = flag.Bool("list", false, "List agents and exit (no TUI)")
	outerSocket = flag.String("socket", "agent-monitor", "Outer tmux socket name for pane control")
	noAttach    = flag.Bool("no-attach", false, "Don't attach to agents on Enter (just list)")
)

// Agent type identifies which coding agent tool is running
type AgentType string

const (
	AgentClaude   AgentType = "claude"
	AgentOpenCode AgentType = "opencode"
	AgentCrush    AgentType = "crush"
	AgentUnknown  AgentType = "unknown"
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

// Agent represents a tracked coding agent (Claude Code, OpenCode, or Crush)
type Agent struct {
	Name      string
	Session   string // tmux session name
	Window    int    // tmux window index
	Pane      int    // tmux pane index
	Type      AgentType
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

// How long to show the "recently finished" indicator
const recentIdleDuration = 30 * time.Minute

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
	spinnerFrame  int      // Animation frame counter
	spinnerActive bool     // Whether a spinner tick chain is running
	showActivity  bool     // Toggle: show last activity line under each agent
	lastActiveAt  map[string]time.Time // session -> when last seen in an active state
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

	statusRecentIdle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("78")) // Teal/green — recently finished

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
		agents:       []Agent{},
		cursor:       0,
		outerSocket:  *outerSocket,
		config:       loadConfig(),
		lastActiveAt: make(map[string]time.Time),
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

// isRecentlyIdle checks if an idle agent was recently in an active state.
func (m Model) isRecentlyIdle(agent Agent) bool {
	if agent.Status != StatusIdle {
		return false
	}
	if t, ok := m.lastActiveAt[agent.Session]; ok {
		return time.Since(t) < recentIdleDuration
	}
	return false
}

// hasRecentlyIdle checks if any idle agent was recently active.
func (m Model) hasRecentlyIdle() bool {
	for _, a := range m.agents {
		if m.isRecentlyIdle(a) {
			return true
		}
	}
	return false
}

// recentIdleAge returns a short human-readable elapsed time string.
func recentIdleAge(since time.Time) string {
	d := time.Since(since)
	if d < time.Minute {
		return "<1m"
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
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

	// First pass: collect all panes, keyed by target
	type paneInfo struct {
		target  string
		command string
	}
	var panes []paneInfo

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}

		target := parts[0]
		command := parts[1]

		if seen[target] {
			continue
		}
		seen[target] = true

		panes = append(panes, paneInfo{target: target, command: command})
	}

	// Second pass: detect coding agents (Claude Code, OpenCode, Crush).
	// Direct match: command contains "claude", "opencode", or "crush"
	// Indirect match: command is something else (e.g. bash/dx wrapper) —
	// probe pane content for UI indicators.
	seenSessions := make(map[string]bool)

	// Version-string pattern: Claude Code sets process.title to its version (e.g. "2.1.39")
	versionCmd := regexp.MustCompile(`^\d+\.\d+\.\d+$`)

	for _, p := range panes {
		agentType := AgentUnknown
		switch {
		case strings.Contains(p.command, "claude"):
			agentType = AgentClaude
		case strings.Contains(p.command, "opencode"):
			agentType = AgentOpenCode
		case strings.Contains(p.command, "crush"):
			agentType = AgentCrush
		case versionCmd.MatchString(p.command):
			// Claude Code sets process.title to its version number
			agentType = AgentClaude
		}

		// Parse session:window.pane
		colonIdx := strings.Index(p.target, ":")
		if colonIdx == -1 {
			continue
		}
		session := p.target[:colonIdx]
		rest := p.target[colonIdx+1:]

		var window, pane int
		fmt.Sscanf(rest, "%d.%d", &window, &pane)

		// For unknown commands, probe pane content for agent UI indicators.
		// Only check one pane per session to avoid overhead.
		if agentType == AgentUnknown {
			if seenSessions[session] {
				continue
			}
			// Try each agent type's content probe in order
			switch {
			case looksLikeClaude(p.target):
				agentType = AgentClaude
			case looksLikeCrush(p.target):
				agentType = AgentCrush
			case looksLikeOpenCode(p.target):
				agentType = AgentOpenCode
			default:
				continue
			}
		}
		seenSessions[session] = true

		agent := Agent{
			Name:      session,
			Session:   session,
			Window:    window,
			Pane:      pane,
			Type:      agentType,
			Status:    StatusUnknown,
			UpdatedAt: time.Now(),
		}

		agent.Status, agent.LastLine = detectAgentStatus(p.target, agentType)
		agents = append(agents, agent)
	}

	// Sort by name
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Name < agents[j].Name
	})

	return agentUpdateMsg(agents)
}

// looksLikeClaude does a quick content probe to detect Claude Code running
// in a wrapper (e.g. dx, container) or when process.title is the version number.
// Checks for distinctive UI elements visible in both idle and active states.
func looksLikeClaude(target string) bool {
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	content := string(output)
	// Idle-state markers
	indicators := []string{
		"Claude Code",
		"⏵⏵",
		"claude.ai/code",
		"ctrl+o",
		"shift+tab to cycle",
	}
	for _, ind := range indicators {
		if strings.Contains(content, ind) {
			return true
		}
	}
	// Active-state markers: tool output, spinners, permission prompts
	// These are visible when Claude is running and the idle prompt has scrolled off.
	activePatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?m)…\s+\(\d+[ms]`),          // spinner with timing: "… (5s"
		regexp.MustCompile(`(?m)^[✻✢✶✦✧✹✺✵✷❋❊⚝*]\s+`),   // spinner prefix chars
		regexp.MustCompile(`(?m)^⎿`),                      // tool result marker
		regexp.MustCompile(`(?m)^●\s+Running\s+\d+`),      // subagent execution
	}
	for _, re := range activePatterns {
		if re.MatchString(content) {
			return true
		}
	}
	// Permission prompts unique to Claude Code
	if strings.Contains(content, "Yes, and don't ask") ||
		strings.Contains(content, "Do you want to proceed?") {
		return true
	}
	return false
}

// looksLikeCrush does a quick content probe to detect Crush running in a wrapper.
func looksLikeCrush(target string) bool {
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	content := string(output)
	indicators := []string{
		"crush>",
		"Crush",
		"crush.json",
		"💘",
	}
	for _, ind := range indicators {
		if strings.Contains(content, ind) {
			return true
		}
	}
	return false
}

// looksLikeOpenCode does a quick content probe to detect OpenCode running in a wrapper.
// OpenCode's Bubble Tea TUI has distinctive bottom-bar elements that are always visible.
func looksLikeOpenCode(target string) bool {
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	content := string(output)
	// Primary: branding text (may not render at all widths)
	for _, ind := range []string{"OpenCode", "opencode>"} {
		if strings.Contains(content, ind) {
			return true
		}
	}
	// Secondary: bottom bar has "ctrl+p commands" AND one of the state indicators.
	// This combination is unique to OpenCode.
	if strings.Contains(content, "ctrl+p commands") &&
		(strings.Contains(content, "tab agents") ||
			strings.Contains(content, "tab switch agent") ||
			strings.Contains(content, "esc interrupt")) {
		return true
	}
	return false
}

// detectAgentStatus dispatches to the appropriate status detector by agent type.
func detectAgentStatus(target string, agentType AgentType) (AgentStatus, string) {
	switch agentType {
	case AgentCrush:
		return detectCrushStatus(target)
	case AgentOpenCode:
		return detectOpenCodeStatus(target)
	default:
		return detectClaudeStatus(target)
	}
}

// detectClaudeStatus captures pane content and determines Claude Code agent state.
func detectClaudeStatus(target string) (AgentStatus, string) {
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
		"⏵",
		"────",
		"❯",
		"@",             // user@host prompt lines
		"Esc to cancel",
		"Tab to amend",
		"Do you want",
		"accept edits",
		"plan mode",
		"Context left",
		"Yes, and don't",
		"Press Enter",
		"ctrl+o",
		"(y/n)",
		"[y/N]",
		"[y/n]",
		"[Y/n]",
		"crush>",
		"opencode>",
		"Ready!",
		"Ready...",
		"Allow",
		"Deny",
		"OpenCode",
		"switch agent",
		"tab agents",
		"ctrl+p",
		"esc interrupt",
		"▀▀▀▀",
		"▣",
		"⬝⬝",
		"■■",
		"tokens",
		"% used",
		"spent",
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

// detectCrushStatus captures pane content and determines Crush agent state.
func detectCrushStatus(target string) (AgentStatus, string) {
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
	output, err := cmd.Output()
	if err != nil {
		return StatusError, ""
	}

	lines := strings.Split(string(output), "\n")

	var lastLines []string
	for i := len(lines) - 1; i >= 0 && len(lastLines) < 20; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			lastLines = append([]string{line}, lastLines...)
		}
	}

	activityLine := findActivityLine(lastLines)
	recentContent := strings.Join(lastLines, "\n")

	// Running: active work indicators
	runningPatterns := []string{
		"Working...", "Thinking...", "Generating...",
		"Processing...", "Brrrrr...", "Prrrrrrrr...",
	}
	for _, pat := range runningPatterns {
		if strings.Contains(recentContent, pat) {
			return StatusRunning, truncate(activityLine, 60)
		}
	}
	// Spinner animation (Bubble Tea spinners)
	if regexp.MustCompile(`(?m)^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s+`).MatchString(recentContent) {
		return StatusRunning, truncate(activityLine, 60)
	}

	// Waiting: permission dialogs
	waitingPatterns := []string{
		"Allow", "Deny",
		"[y/n]", "[Y/n]", "(y/n)",
	}
	for _, pat := range waitingPatterns {
		if strings.Contains(recentContent, pat) {
			return StatusWaiting, truncate(activityLine, 60)
		}
	}

	// Idle: at prompt or ready
	var lastLine string
	if len(lastLines) > 0 {
		lastLine = lastLines[len(lastLines)-1]
	}
	if strings.Contains(lastLine, "crush>") ||
		strings.Contains(recentContent, "Ready!") ||
		strings.Contains(recentContent, "Ready...") ||
		strings.Contains(recentContent, "Ready for instructions") {
		return StatusIdle, truncate(activityLine, 60)
	}

	return StatusIdle, truncate(activityLine, 60)
}

// detectOpenCodeStatus captures pane content and determines OpenCode agent state.
// OpenCode is a Bubble Tea TUI with a status bar at bottom showing "• OpenCode X.Y.Z".
// Running state indicators:
//   - Progress bar with ■/⬝ characters and "esc interrupt" in the bottom bar
//   - Tool call lines prefixed with ✱ (e.g. "✱ Glob ...")
// Idle state: bottom bar shows "tab switch agent" / "tab agents"
func detectOpenCodeStatus(target string) (AgentStatus, string) {
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
	output, err := cmd.Output()
	if err != nil {
		return StatusError, ""
	}

	content := string(output)
	lines := strings.Split(content, "\n")

	var lastLines []string
	for i := len(lines) - 1; i >= 0 && len(lastLines) < 20; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			lastLines = append([]string{line}, lastLines...)
		}
	}

	activityLine := findActivityLine(lastLines)
	recentContent := strings.Join(lastLines, "\n")

	// Running: "esc interrupt" in bottom bar — definitive running indicator
	if strings.Contains(recentContent, "esc interrupt") {
		return StatusRunning, truncate(activityLine, 60)
	}
	// Running: progress bar with filled/empty squares
	if strings.Contains(recentContent, "■") || strings.Contains(recentContent, "⬝") {
		return StatusRunning, truncate(activityLine, 60)
	}
	// Running: tool call lines (✱ prefix)
	if regexp.MustCompile(`(?m)^✱\s+`).MatchString(recentContent) {
		return StatusRunning, truncate(activityLine, 60)
	}
	// Running: explicit status text
	runningPatterns := []string{
		"Working...", "Thinking...", "Processing...",
	}
	for _, pat := range runningPatterns {
		if strings.Contains(recentContent, pat) {
			return StatusRunning, truncate(activityLine, 60)
		}
	}

	// Waiting: permission dialogs
	waitingPatterns := []string{
		"[y/n]", "[Y/n]", "(y/n)",
		"allow", "deny",
	}
	for _, pat := range waitingPatterns {
		if strings.Contains(recentContent, pat) {
			return StatusWaiting, truncate(activityLine, 60)
		}
	}

	// Idle: default state (OpenCode TUI is visible but not actively working)
	return StatusIdle, truncate(activityLine, 60)
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

		case key.Matches(msg, keys.ToggleActivity):
			m.showActivity = !m.showActivity
		}

	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			idx := m.mouseYToAgentIndex(msg.Y)
			if idx >= 0 && idx < len(m.flatAgents) {
				m.cursor = idx
				// Immediately attach on click
				if !*noAttach {
					return m, m.attachToAgent(m.flatAgents[idx])
				}
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(tickCmd(), detectAgents)

	case spinnerTickMsg:
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		// Keep ticking while there are agents (for title shimmer + status animations)
		if len(m.agents) > 0 {
			return m, spinnerTickCmd()
		}
		m.spinnerActive = false
		return m, nil

	case agentUpdateMsg:
		m.agents = []Agent(msg)
		// Track when agents are active for "recently idle" indicator
		for _, a := range m.agents {
			if a.Status == StatusRunning || a.Status == StatusPlanning || a.Status == StatusWaiting {
				m.lastActiveAt[a.Session] = time.Now()
			}
		}
		m.buildGroups()
		if m.cursor >= len(m.flatAgents) && len(m.flatAgents) > 0 {
			m.cursor = len(m.flatAgents) - 1
		}
		// Start spinner tick chain if not already running
		if !m.spinnerActive && len(m.agents) > 0 {
			m.spinnerActive = true
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

// agentLineHeight returns how many lines an agent occupies.
func (m Model) agentLineHeight(agent Agent) int {
	if m.showActivity && agent.LastLine != "" {
		return 2
	}
	return 1
}

// mouseYToAgentIndex converts a mouse Y coordinate to a flatAgents index,
// dynamically computing the offset by replaying the View layout.
func (m Model) mouseYToAgentIndex(y int) int {
	// Replay the layout to compute line positions:
	// Y=0: panel top border
	// Y=1: title line
	// Y=2: blank line
	// Y=3+: agents/groups
	line := 1 // start after top border
	line++    // title
	line++    // blank line

	agentIdx := 0
	if m.groups != nil {
		for _, g := range m.groups {
			if y == line {
				return -1 // clicked on header
			}
			line++ // group header
			for _, agent := range g.Agents {
				h := m.agentLineHeight(agent)
				if y >= line && y < line+h {
					return agentIdx
				}
				line += h
				agentIdx++
			}
		}
	} else {
		for i, agent := range m.flatAgents {
			h := m.agentLineHeight(agent)
			if y >= line && y < line+h {
				return i
			}
			line += h
		}
	}
	return -1
}

// stripAnsi removes ANSI escape sequences from a string.
func stripAnsi(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
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
		if m.isRecentlyIdle(agent) {
			return statusRecentIdle.Render("✓")
		}
		return statusIdle.Render(agent.Status.Symbol())
	case StatusError:
		return statusError.Render(agent.Status.Symbol())
	default:
		return agent.Status.Symbol()
	}
}

// renderAgentLine renders an agent line with status symbol and name,
// plus an optional second line showing last activity in dim text.
func (m Model) renderAgentLine(agent Agent, idx int, maxNameLen int) string {
	symbol := m.renderStatusSymbol(agent)

	name := agent.Name
	if agent.Target() == m.attached {
		name = attachedStyle.Render(name + " ◀")
	}

	// Add "done Xm ago" suffix for recently idle agents
	suffix := ""
	if m.isRecentlyIdle(agent) {
		age := recentIdleAge(m.lastActiveAt[agent.Session])
		suffix = " " + dimStyle.Render(age)
	}

	line := fmt.Sprintf("%s %s%s", symbol, name, suffix)

	if idx == m.cursor {
		line = selectedStyle.Render("> " + line)
	} else {
		line = normalStyle.Render("  " + line)
	}

	// Second line: last activity (only when toggled on)
	if m.showActivity && agent.LastLine != "" {
		// Truncate to fit panel width: panel border (2) + padding (2) + indent (6)
		maxActivity := m.width - 12
		if maxActivity < 10 {
			maxActivity = 10
		}
		activity := truncate(agent.LastLine, maxActivity)
		line += "\n" + dimStyle.Render("      "+activity)
	}

	return line
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

// renderStatusSummary renders a compact colored status summary line using symbols.
// Format: ●2 ◇1 ◐1 ✓3 ○5  (only non-zero counts shown)
func (m Model) renderStatusSummary() string {
	var running, planning, waiting, idle, recentIdle, errCount int
	for _, a := range m.flatAgents {
		switch a.Status {
		case StatusRunning:
			running++
		case StatusPlanning:
			planning++
		case StatusWaiting:
			waiting++
		case StatusIdle:
			if m.isRecentlyIdle(a) {
				recentIdle++
			} else {
				idle++
			}
		case StatusError:
			errCount++
		}
	}

	var parts []string
	if running > 0 {
		parts = append(parts, statusRunning.Render(fmt.Sprintf("●%d", running)))
	}
	if planning > 0 {
		parts = append(parts, statusPlanning.Render(fmt.Sprintf("◇%d", planning)))
	}
	if waiting > 0 {
		parts = append(parts, statusWaiting.Render(fmt.Sprintf("◐%d", waiting)))
	}
	if recentIdle > 0 {
		parts = append(parts, statusRecentIdle.Render(fmt.Sprintf("✓%d", recentIdle)))
	}
	if idle > 0 {
		parts = append(parts, statusIdle.Render(fmt.Sprintf("○%d", idle)))
	}
	if errCount > 0 {
		parts = append(parts, statusError.Render(fmt.Sprintf("✕%d", errCount)))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
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
	content.WriteString("\n\n")

	if len(m.flatAgents) == 0 {
		content.WriteString(normalStyle.Render("No agents found.\n"))
		content.WriteString(dimStyle.Render("Start an agent in tmux.\n"))
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

	// Status summary at the bottom of the agent list
	if len(m.flatAgents) > 0 {
		summary := m.renderStatusSummary()
		if summary != "" {
			content.WriteString(dimStyle.Render("─") + " " + summary)
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
	helpKeys := "j/k:nav  ⏎:attach  l:focus  a:activity  r:refresh  q:quit"
	helpText := helpStyle.Render(helpKeys) + "  " + dimStyle.Render(version)
	helpPanel := helpPanelStyle.Width(panelWidth).Render(helpText)

	return agentPanel + "\n" + helpPanel
}

// Key bindings
type keyMap struct {
	Up             key.Binding
	Down           key.Binding
	Attach         key.Binding
	FocusRight     key.Binding
	Refresh        key.Binding
	ToggleActivity key.Binding
	Quit           key.Binding
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
	ToggleActivity: key.NewBinding(
		key.WithKeys("a"),
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
			fmt.Println("No agents detected.")
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
