package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
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
	listOnly          = flag.Bool("list", false, "List agents and exit (no TUI)")
	outerSocket       = flag.String("socket", "agent-monitor", "Outer tmux socket name for pane control")
	noAttach          = flag.Bool("no-attach", false, "Don't attach to agents on Enter (just list)")
	groupsFlag        = flag.String("groups", "", "Comma-separated list of group names to show (default: all)")
	autoApprovePlans  = flag.Bool("auto-approve-plans", false, "Automatically approve plan mode exits for Claude agents")
	notifyOSC         = flag.Bool("notify", false, "Enable OSC 9 terminal notifications (passthrough to terminal emulator)")
	ntfyTopic         = flag.String("ntfy-topic", "", "Enable ntfy.sh push notifications to this topic")
	ntfyServer        = flag.String("ntfy-server", "https://ntfy.sh", "ntfy server URL")
	notifyCmd         = flag.String("notify-cmd", "", "Run custom command on notification (env: AGENT_MONITOR_AGENT, _BADGE, _EVENT, _TITLE, _MESSAGE)")
)

// Agent type identifies which coding agent tool is running
type AgentType string

const (
	AgentClaude   AgentType = "claude"
	AgentOpenCode AgentType = "opencode"
	AgentCrush    AgentType = "crush"
	AgentCodex    AgentType = "codex"
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

// AgentPresence indicates whether the agent is real or a placeholder
type AgentPresence int

const (
	PresenceActive    AgentPresence = iota // Agent detected and running
	PresenceNoAgent                        // Tmux session exists, no agent process
	PresenceNoSession                      // Session defined in config but not running
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

// Badge returns a short label identifying the agent type.
func (t AgentType) Badge() string {
	switch t {
	case AgentClaude:
		return "cc"
	case AgentCodex:
		return "cx"
	case AgentOpenCode:
		return "oc"
	case AgentCrush:
		return "cr"
	default:
		return "??"
	}
}

// BadgeStyle returns the color style for this agent type's badge.
func (t AgentType) BadgeStyle() lipgloss.Style {
	switch t {
	case AgentClaude:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#b388ff")) // light purple
	case AgentCodex:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#66bb6a")) // green
	case AgentOpenCode:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#4fc3f7")) // cyan
	case AgentCrush:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#ef5350")) // red/pink
	default:
		return dimStyle
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
	Presence  AgentPresence // Active, NoAgent (session exists), or NoSession
	LastLine  string        // Last line of output (for status detection)
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

// SubGroup holds agents that share a slash-prefix (e.g. "code/branch1", "code/main")
type SubGroup struct {
	Prefix string
	Agents []Agent
}

// GroupItem is either a single agent or a sub-group of agents
type GroupItem struct {
	IsSubGroup bool
	Agent      Agent    // when IsSubGroup=false
	SubGroup   SubGroup // when IsSubGroup=true
}

// Group holds a named group of agents for display
type Group struct {
	Name  string
	Items []GroupItem
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

// favoritesPath returns the path to the favorites file.
func favoritesPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agent-monitor", "favorites.json")
}

// loadFavorites reads favorited session names from disk.
func loadFavorites() map[string]bool {
	path := favoritesPath()
	if path == "" {
		return make(map[string]bool)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return make(map[string]bool)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return make(map[string]bool)
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// saveFavorites writes favorited session names to disk.
func saveFavorites(favs map[string]bool) {
	path := favoritesPath()
	if path == "" {
		return
	}
	var names []string
	for n := range favs {
		names = append(names, n)
	}
	sort.Strings(names)
	data, err := json.Marshal(names)
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0644)
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

// Toast represents a temporary notification passed to tmux display-message
type Toast struct {
	AgentName string
	Message   string
	Badge     string
}

// NotifyEvent identifies what triggered a desktop notification.
type NotifyEvent string

const (
	NotifyWaiting  NotifyEvent = "waiting"
	NotifyFinished NotifyEvent = "finished"
)

// Notification carries the data needed by all notification backends.
type Notification struct {
	AgentName string
	Badge     string
	Event     NotifyEvent
	Message   string
}

func (n Notification) Title() string {
	switch n.Event {
	case NotifyWaiting:
		return fmt.Sprintf("[%s] %s needs input", n.Badge, n.AgentName)
	case NotifyFinished:
		return fmt.Sprintf("[%s] %s finished", n.Badge, n.AgentName)
	default:
		return fmt.Sprintf("[%s] %s", n.Badge, n.AgentName)
	}
}

func (n Notification) Body() string {
	if n.Message != "" {
		return truncate(n.Message, 80)
	}
	return string(n.Event)
}

// sendOSCNotification writes an OSC 777 escape sequence that reaches the
// terminal emulator (e.g. Ghostty) through nested tmux layers.
// It wraps the OSC in a DCS tmux passthrough so the inner tmux (default socket)
// forwards it to the real terminal. Requires allow-passthrough on.
func sendOSCNotification(n Notification, outerSocket string) tea.Cmd {
	return func() tea.Msg {
		osc := fmt.Sprintf("\033]777;notify;%s;%s\007", n.Title(), n.Body())
		// DCS passthrough: double each ESC in the payload
		dcs := "\033Ptmux;" + strings.ReplaceAll(osc, "\033", "\033\033") + "\033\\"

		// Write DCS-wrapped sequence to the outer tmux client's PTY.
		// That PTY is a pane in the inner tmux, which (with allow-passthrough on)
		// unwraps the DCS and forwards the raw OSC 777 to the real terminal.
		if outerSocket != "" {
			cmd := exec.Command("tmux", "-L", outerSocket, "list-clients", "-F", "#{client_tty}")
			out, err := cmd.Output()
			if err == nil {
				tty := strings.TrimSpace(string(out))
				if tty != "" {
					tty = strings.SplitN(tty, "\n", 2)[0]
					if f, err := os.OpenFile(tty, os.O_WRONLY, 0); err == nil {
						f.WriteString(dcs)
						f.Close()
						return nil
					}
				}
			}
		}
		// Fallback: write raw OSC to /dev/tty (no tmux nesting)
		if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
			f.WriteString(osc)
			f.Close()
		}
		return nil
	}
}

// sendNtfyNotification POSTs a notification to an ntfy server.
func sendNtfyNotification(n Notification, server, topic string) tea.Cmd {
	return func() tea.Msg {
		url := strings.TrimRight(server, "/") + "/" + topic
		body := bytes.NewBufferString(n.Body())
		req, err := http.NewRequest("POST", url, body)
		if err != nil {
			return nil
		}
		req.Header.Set("Title", n.Title())
		if n.Event == NotifyWaiting {
			req.Header.Set("Priority", "high")
			req.Header.Set("Tags", "warning")
		} else {
			req.Header.Set("Priority", "default")
			req.Header.Set("Tags", "white_check_mark")
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil
	}
}

// sendCmdNotification runs a user-provided command with notification env vars.
func sendCmdNotification(n Notification, cmdStr string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Env = append(os.Environ(),
			"AGENT_MONITOR_AGENT="+n.AgentName,
			"AGENT_MONITOR_BADGE="+n.Badge,
			"AGENT_MONITOR_EVENT="+string(n.Event),
			"AGENT_MONITOR_TITLE="+n.Title(),
			"AGENT_MONITOR_MESSAGE="+n.Message,
		)
		cmd.Run()
		return nil
	}
}

// dispatchNotification fans out a notification to all enabled backends.
func dispatchNotification(n Notification, outerSocket string) []tea.Cmd {
	var cmds []tea.Cmd
	if *notifyOSC {
		cmds = append(cmds, sendOSCNotification(n, outerSocket))
	}
	if *ntfyTopic != "" {
		cmds = append(cmds, sendNtfyNotification(n, *ntfyServer, *ntfyTopic))
	}
	if *notifyCmd != "" {
		cmds = append(cmds, sendCmdNotification(n, *notifyCmd))
	}
	return cmds
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
	spinnerFrame  int      // Animation frame counter
	spinnerActive bool     // Whether a spinner tick chain is running
	showActivity   bool     // Toggle: show last activity line under each agent
	lastActiveAt   map[string]time.Time // session -> when last seen in an active state
	filterGroups        map[string]bool        // If non-nil, only show these group names
	previousStatus      map[string]AgentStatus // session -> last known status (for transition detection)
	planPendingApproval map[string]bool        // sessions seen in planning, awaiting auto-approval
	favorites           map[string]bool        // session name -> is favorite
	filterFavorites     bool                   // when true, only show favorited agents
	scrollOffset        int                    // viewport scroll offset for agent list
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

	// Phantom session styles
	phantomNoAgentStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("243")) // medium gray — session running, no agent

	phantomNoSessionStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("237")) // dark gray — session not running

	favoriteStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("220")) // yellow star
)

func initialModel() Model {
	m := Model{
		agents:              []Agent{},
		cursor:              0,
		outerSocket:         *outerSocket,
		config:              loadConfig(),
		lastActiveAt:        make(map[string]time.Time),
		previousStatus:      make(map[string]AgentStatus),
		planPendingApproval: make(map[string]bool),
		favorites:           loadFavorites(),
	}
	if *groupsFlag != "" {
		m.filterGroups = make(map[string]bool)
		for _, name := range strings.Split(*groupsFlag, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				m.filterGroups[name] = true
			}
		}
	}
	return m
}

// computeItems organizes a list of agents into GroupItems, bundling
// consecutive agents that share a slash-prefix into SubGroups.
func computeItems(agents []Agent) []GroupItem {
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Name < agents[j].Name
	})

	var items []GroupItem
	i := 0
	for i < len(agents) {
		slashIdx := strings.Index(agents[i].Name, "/")
		if slashIdx == -1 {
			// No slash — plain agent
			items = append(items, GroupItem{Agent: agents[i]})
			i++
			continue
		}
		// Collect all agents with the same prefix
		prefix := agents[i].Name[:slashIdx]
		var subAgents []Agent
		for i < len(agents) && strings.HasPrefix(agents[i].Name, prefix+"/") {
			subAgents = append(subAgents, agents[i])
			i++
		}
		items = append(items, GroupItem{
			IsSubGroup: true,
			SubGroup:   SubGroup{Prefix: prefix, Agents: subAgents},
		})
	}
	return items
}

// flattenItems extracts agents from items in display order.
func flattenItems(items []GroupItem) []Agent {
	var out []Agent
	for _, item := range items {
		if item.IsSubGroup {
			out = append(out, item.SubGroup.Agents...)
		} else {
			out = append(out, item.Agent)
		}
	}
	return out
}

// buildGroups maps agents into config-defined groups and computes flatAgents.
func (m *Model) buildGroups() {
	// Apply favorites filter if active
	agents := m.agents
	if m.filterFavorites {
		var filtered []Agent
		for _, a := range agents {
			if m.favorites[a.Session] {
				filtered = append(filtered, a)
			}
		}
		agents = filtered
	}

	if len(m.config.Groups) == 0 {
		// No config — flat list with sub-groups but no group headers
		if len(agents) == 0 {
			m.groups = nil
			m.flatAgents = nil
			return
		}
		items := computeItems(agents)
		m.groups = []Group{{Name: "", Items: items}}
		m.flatAgents = flattenItems(items)
		return
	}

	// Build lookups: exact session name -> group index, and wildcard prefixes
	exactMatch := make(map[string]int)
	type wildcardEntry struct {
		prefix   string
		groupIdx int
	}
	var wildcards []wildcardEntry

	for i, gc := range m.config.Groups {
		for _, s := range gc.Sessions {
			if strings.HasSuffix(s, "/*") {
				wildcards = append(wildcards, wildcardEntry{
					prefix:   strings.TrimSuffix(s, "/*"),
					groupIdx: i,
				})
			} else {
				exactMatch[s] = i
			}
		}
	}

	// Bucket agents into groups
	buckets := make([][]Agent, len(m.config.Groups))
	var other []Agent
	for _, agent := range agents {
		if idx, ok := exactMatch[agent.Session]; ok {
			buckets[idx] = append(buckets[idx], agent)
		} else {
			matched := false
			for _, wc := range wildcards {
				if strings.HasPrefix(agent.Session, wc.prefix+"/") {
					buckets[wc.groupIdx] = append(buckets[wc.groupIdx], agent)
					matched = true
					break
				}
			}
			if !matched {
				other = append(other, agent)
			}
		}
	}

	// Build groups preserving config order, skip empty groups
	m.groups = nil
	m.flatAgents = nil
	for i, gc := range m.config.Groups {
		if len(buckets[i]) == 0 {
			continue
		}
		if m.filterGroups != nil && !m.filterGroups[gc.Name] {
			continue
		}
		items := computeItems(buckets[i])
		g := Group{Name: gc.Name, Items: items}
		m.groups = append(m.groups, g)
		m.flatAgents = append(m.flatAgents, flattenItems(items)...)
	}
	if len(other) > 0 && m.filterGroups == nil {
		items := computeItems(other)
		g := Group{Name: "Other", Items: items}
		m.groups = append(m.groups, g)
		m.flatAgents = append(m.flatAgents, flattenItems(items)...)
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
		case strings.Contains(p.command, "codex"):
			agentType = AgentCodex
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
			case looksLikeCodex(p.target):
				agentType = AgentCodex
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

// looksLikeCodex does a quick content probe to detect OpenAI Codex running in a pane.
func looksLikeCodex(target string) bool {
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	content := string(output)
	indicators := []string{
		"OpenAI Codex",
		"codex",
		"? for shortcuts",
		"context left",
	}
	for _, ind := range indicators {
		if strings.Contains(content, ind) {
			return true
		}
	}
	return false
}

// detectCodexStatus captures pane content and determines OpenAI Codex agent state.
// Codex is a Node.js TUI with a box-drawn header showing "OpenAI Codex".
// Idle state: "›" prompt visible, "? for shortcuts", "context left" in bottom bar.
func detectCodexStatus(target string) (AgentStatus, string) {
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

	var lastLine string
	if len(lastLines) > 0 {
		lastLine = lastLines[len(lastLines)-1]
	}

	// Idle check first — the "›" prompt and bottom bar are definitive.
	// Must check before running patterns because "• Ran" from previous
	// tool executions persists in scrollback after Codex returns to idle.
	isIdle := strings.Contains(lastLine, "›") ||
		strings.Contains(recentContent, "? for shortcuts") ||
		strings.Contains(recentContent, "context left")

	// Running: active thinking/working with timing — "• Something (19s • esc to interrupt)"
	if strings.Contains(recentContent, "esc to interrupt") {
		return StatusRunning, truncate(activityLine, 60)
	}

	if !isIdle {
		// Running: tool execution lines — "• Ran ..." at line start
		if regexp.MustCompile(`(?m)^• Ran\s+`).MatchString(recentContent) {
			return StatusRunning, truncate(activityLine, 60)
		}
		// Running: spinner or active work indicators
		if regexp.MustCompile(`(?m)^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s+`).MatchString(recentContent) {
			return StatusRunning, truncate(activityLine, 60)
		}
		runningPatterns := []string{
			"Working...", "Thinking...", "Generating...",
			"Processing...", "Executing...",
		}
		for _, pat := range runningPatterns {
			if strings.Contains(recentContent, pat) {
				return StatusRunning, truncate(activityLine, 60)
			}
		}
		// Running: sandbox execution
		if strings.Contains(recentContent, "Running command") ||
			strings.Contains(recentContent, "Applying patch") {
			return StatusRunning, truncate(activityLine, 60)
		}
	}

	// Waiting: permission / approval prompts
	waitingPatterns := []string{
		"[y/n]", "[Y/n]", "(y/n)",
		"Allow", "Deny",
		"approve", "reject",
	}
	for _, pat := range waitingPatterns {
		if strings.Contains(recentContent, pat) {
			return StatusWaiting, truncate(activityLine, 60)
		}
	}

	if isIdle {
		return StatusIdle, truncate(activityLine, 60)
	}

	return StatusIdle, truncate(activityLine, 60)
}

// detectAgentStatus dispatches to the appropriate status detector by agent type.
func detectAgentStatus(target string, agentType AgentType) (AgentStatus, string) {
	switch agentType {
	case AgentCrush:
		return detectCrushStatus(target)
	case AgentOpenCode:
		return detectOpenCodeStatus(target)
	case AgentCodex:
		return detectCodexStatus(target)
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
		"OpenAI Codex",
		"? for shortcuts",
		"context left",
		"codex",
		"esc to interrupt",
		"• Ran ",
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
			// Move up, skipping non-selectable (PresenceNoSession) entries
			newCursor := m.cursor - 1
			for newCursor >= 0 && m.flatAgents[newCursor].Presence == PresenceNoSession {
				newCursor--
			}
			if newCursor >= 0 {
				m.cursor = newCursor
				m.ensureCursorVisible()
			}

		case key.Matches(msg, keys.Down):
			// Move down, skipping non-selectable (PresenceNoSession) entries
			newCursor := m.cursor + 1
			for newCursor < len(m.flatAgents) && m.flatAgents[newCursor].Presence == PresenceNoSession {
				newCursor++
			}
			if newCursor < len(m.flatAgents) {
				m.cursor = newCursor
				m.ensureCursorVisible()
			}

		case key.Matches(msg, keys.Attach):
			if len(m.flatAgents) > 0 && m.cursor < len(m.flatAgents) && !*noAttach {
				agent := m.flatAgents[m.cursor]
				if agent.Presence != PresenceNoSession {
					return m, m.attachToAgent(agent)
				}
			}

		case key.Matches(msg, keys.FocusRight):
			return m, m.focusRightPane()

		case key.Matches(msg, keys.Refresh):
			return m, detectAgents

		case key.Matches(msg, keys.ToggleActivity):
			m.showActivity = !m.showActivity
			m.ensureCursorVisible()

		case key.Matches(msg, keys.ToggleFavorite):
			if len(m.flatAgents) > 0 && m.cursor < len(m.flatAgents) {
				session := m.flatAgents[m.cursor].Session
				if m.favorites[session] {
					delete(m.favorites, session)
				} else {
					m.favorites[session] = true
				}
				saveFavorites(m.favorites)
			}

		case key.Matches(msg, keys.FilterFavorites):
			m.filterFavorites = !m.filterFavorites
			m.scrollOffset = 0
			m.buildGroups()
			if m.cursor >= len(m.flatAgents) && len(m.flatAgents) > 0 {
				m.cursor = len(m.flatAgents) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.ensureCursorVisible()
		}

	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			idx := m.mouseYToAgentIndex(msg.Y)
			if idx >= 0 && idx < len(m.flatAgents) && m.flatAgents[idx].Presence != PresenceNoSession {
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
		// Detect transitions and create toasts + desktop notifications
		var toastCmds []tea.Cmd
		for _, a := range m.agents {
			// Skip notifications for the currently attached/focused agent
			if a.Target() == m.attached {
				continue
			}
			prev, known := m.previousStatus[a.Session]
			if !known {
				continue
			}
			// Waiting transition: tmux toast + desktop notification
			if prev != StatusWaiting && a.Status == StatusWaiting {
				toast := Toast{
					AgentName: a.Name,
					Message:   a.LastLine,
					Badge:     a.Type.Badge(),
				}
				if m.outerSocket != "" {
					toastCmds = append(toastCmds, m.tmuxDisplayToast(toast))
				}
				toastCmds = append(toastCmds, dispatchNotification(Notification{
					AgentName: a.Name,
					Badge:     a.Type.Badge(),
					Event:     NotifyWaiting,
					Message:   a.LastLine,
				}, m.outerSocket)...)
			}
			// Finished transition: running/planning → idle (desktop notification only)
			if (prev == StatusRunning || prev == StatusPlanning) && a.Status == StatusIdle {
				toastCmds = append(toastCmds, dispatchNotification(Notification{
					AgentName: a.Name,
					Badge:     a.Type.Badge(),
					Event:     NotifyFinished,
					Message:   a.LastLine,
				}, m.outerSocket)...)
			}
		}
		// Rebuild previousStatus from current agents (cleans up stale entries)
		newStatus := make(map[string]AgentStatus, len(m.agents))
		for _, a := range m.agents {
			newStatus[a.Session] = a.Status
		}
		m.previousStatus = newStatus
		// Auto-approve plan mode exits for Claude agents
		if *autoApprovePlans {
			for _, a := range m.agents {
				if a.Type != AgentClaude {
					continue
				}
				if a.Status == StatusPlanning {
					m.planPendingApproval[a.Session] = true
				} else if m.planPendingApproval[a.Session] {
					if a.Status == StatusIdle || a.Status == StatusWaiting {
						delete(m.planPendingApproval, a.Session)
						toastCmds = append(toastCmds, sendPlanApproval(a, m.outerSocket))
					} else if a.Status != StatusRunning {
						// Clear for error/unknown — Running preserves the flag
						// because ExitPlanMode response may still be rendering
						delete(m.planPendingApproval, a.Session)
					}
				}
			}
		}
		// Track when agents are active for "recently idle" indicator
		for _, a := range m.agents {
			if a.Status == StatusRunning || a.Status == StatusPlanning || a.Status == StatusWaiting {
				m.lastActiveAt[a.Session] = time.Now()
			}
		}
		// Inject phantom sessions from groups config
		if len(m.config.Groups) > 0 {
			m.agents = append(m.agents, detectPhantomSessions(m.config, m.agents)...)
		}
		m.buildGroups()
		if m.cursor >= len(m.flatAgents) && len(m.flatAgents) > 0 {
			m.cursor = len(m.flatAgents) - 1
		}
		// Ensure cursor is on a selectable entry
		m.snapCursorToSelectable()
		m.ensureCursorVisible()
		// Collect cmds: spinner + any toast display-message commands
		var cmds []tea.Cmd
		if !m.spinnerActive && len(m.agents) > 0 {
			m.spinnerActive = true
			cmds = append(cmds, spinnerTickCmd())
		}
		cmds = append(cmds, toastCmds...)
		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
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
// ensureCursorVisible adjusts scrollOffset so the cursor is within the viewport.
// cursorLine is computed by walking the display structure to find the line
// corresponding to m.cursor in the agent list.
func (m *Model) ensureCursorVisible() {
	// Compute which line the cursor agent occupies
	line := 0
	agentIdx := 0
	for _, g := range m.groups {
		if g.Name != "" {
			line++ // group header
		}
		for _, item := range g.Items {
			if item.IsSubGroup {
				line++ // sub-group header
				for _, agent := range item.SubGroup.Agents {
					if agentIdx == m.cursor {
						goto found
					}
					line++ // agent line
					if m.showActivity && agent.LastLine != "" {
						line++ // activity line
					}
					agentIdx++
				}
			} else {
				if agentIdx == m.cursor {
					goto found
				}
				line++
				if m.showActivity && item.Agent.LastLine != "" {
					line++
				}
				agentIdx++
			}
		}
	}
found:
	// Available height for list (same calc as View)
	overhead := 7 // title(1) + blank(1) + borders(2) + help(3)
	if m.scrollOffset > 0 {
		overhead++ // "↑ more" line
	}
	availHeight := m.height - overhead
	if availHeight < 3 {
		availHeight = 3
	}

	if line < m.scrollOffset {
		m.scrollOffset = line
	}
	if line >= m.scrollOffset+availHeight {
		m.scrollOffset = line - availHeight + 1
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

// snapCursorToSelectable ensures the cursor is on a selectable entry.
func (m *Model) snapCursorToSelectable() {
	if len(m.flatAgents) == 0 {
		return
	}
	// Try current position first
	if m.cursor < len(m.flatAgents) && m.flatAgents[m.cursor].Presence != PresenceNoSession {
		return
	}
	// Search forward, then backward
	for i := m.cursor; i < len(m.flatAgents); i++ {
		if m.flatAgents[i].Presence != PresenceNoSession {
			m.cursor = i
			return
		}
	}
	for i := m.cursor - 1; i >= 0; i-- {
		if m.flatAgents[i].Presence != PresenceNoSession {
			m.cursor = i
			return
		}
	}
}

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
	for _, g := range m.groups {
		// Group header (skip for empty name — flat mode)
		if g.Name != "" {
			if y == line {
				return -1 // clicked on group header
			}
			line++
		}

		for _, item := range g.Items {
			if item.IsSubGroup {
				// Sub-group header line
				if y == line {
					return -1 // clicked on sub-group header
				}
				line++
				for _, agent := range item.SubGroup.Agents {
					h := m.agentLineHeight(agent)
					if y >= line && y < line+h {
						return agentIdx
					}
					line += h
					agentIdx++
				}
			} else {
				h := m.agentLineHeight(item.Agent)
				if y >= line && y < line+h {
					return agentIdx
				}
				line += h
				agentIdx++
			}
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

		// For phantom sessions (no agent), attach to the session itself
		attachTarget := target
		if agent.Presence == PresenceNoAgent {
			attachTarget = agent.Session
		}

		// Use respawn-pane to kill whatever is running and start fresh with the attach command
		// This cleanly handles: placeholder, attached tmux session, or anything else
		attachCmd := fmt.Sprintf("unset TMUX; exec tmux attach-session -t '%s'", attachTarget)
		exec.Command("tmux", "-L", m.outerSocket, "respawn-pane", "-k", "-t", "0.1", attachCmd).Run()

		// Focus the right pane
		exec.Command("tmux", "-L", m.outerSocket, "select-pane", "-t", "0.1").Run()

		return attachResultMsg{target: target, err: nil}
	}
}

// tmuxDisplayToast fires a tmux display-message on the outer socket so the
// notification is visible across both panes (even when the right pane has focus).
func (m Model) tmuxDisplayToast(toast Toast) tea.Cmd {
	return func() tea.Msg {
		msg := fmt.Sprintf("[%s] %s needs input", toast.Badge, toast.AgentName)
		if toast.Message != "" {
			msg += " — " + truncate(toast.Message, 40)
		}
		exec.Command("tmux", "-L", m.outerSocket,
			"display-message", "-d", "6000", msg).Run()
		return nil
	}
}

// sendPlanApproval sends keystrokes to approve a plan exit and notifies via outer tmux.
func sendPlanApproval(agent Agent, outerSocket string) tea.Cmd {
	return func() tea.Msg {
		target := agent.Target()
		if agent.Status == StatusIdle {
			// At the ⏵⏵ prompt — type "yes" to approve the plan
			exec.Command("tmux", "send-keys", "-t", target, "yes", "Enter").Run()
		} else {
			// At an approval prompt — press Enter to accept default
			exec.Command("tmux", "send-keys", "-t", target, "Enter").Run()
		}
		if outerSocket != "" {
			msg := fmt.Sprintf("[%s] %s — auto-approved plan", agent.Type.Badge(), agent.Name)
			exec.Command("tmux", "-L", outerSocket, "display-message", "-d", "4000", msg).Run()
		}
		return nil
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
// If displayName is non-empty, it is shown instead of agent.Name (for sub-grouped agents).
// indent is prepended before the cursor/selection prefix (used for sub-group nesting).
func (m Model) renderAgentLine(agent Agent, idx int, maxNameLen int, displayName string, indent string) string {
	// Favorite indicator (trailing)
	favSuffix := ""
	if m.favorites[agent.Session] {
		favSuffix = " " + favoriteStyle.Render("★")
	}

	// Phantom agents: use "·" in place of status symbol, "  " for missing badge
	if agent.Presence == PresenceNoSession {
		name := agent.Name
		if displayName != "" {
			name = displayName
		}
		return phantomNoSessionStyle.Render(indent + "  ·    " + name + favSuffix)
	}
	if agent.Presence == PresenceNoAgent {
		name := agent.Name
		if displayName != "" {
			name = displayName
		}
		if idx == m.cursor {
			return selectedStyle.Render(indent + "> ·    " + name + favSuffix)
		}
		return indent + "  " + phantomNoAgentStyle.Render("·    "+name) + favSuffix
	}

	// Active agent rendering
	symbol := m.renderStatusSymbol(agent)

	name := agent.Name
	if displayName != "" {
		name = displayName
	}
	if agent.Target() == m.attached {
		name = attachedStyle.Render(name + " ◀")
	}

	// Add "done Xm ago" suffix for recently idle agents
	suffix := ""
	if m.isRecentlyIdle(agent) {
		age := recentIdleAge(m.lastActiveAt[agent.Session])
		suffix = " " + dimStyle.Render(age)
	}

	badge := agent.Type.BadgeStyle().Render(agent.Type.Badge())
	line := fmt.Sprintf("%s %s %s%s%s", symbol, badge, name, suffix, favSuffix)

	if idx == m.cursor {
		line = selectedStyle.Render(indent + "> " + line)
	} else {
		line = normalStyle.Render(indent + "  " + line)
	}

	// Second line: last activity (only when toggled on)
	if m.showActivity && agent.LastLine != "" {
		// Truncate to fit panel width: panel border (2) + padding (2) + indent (6) + extra indent
		maxActivity := m.width - 12 - len(indent)
		if maxActivity < 10 {
			maxActivity = 10
		}
		activity := truncate(agent.LastLine, maxActivity)
		line += "\n" + dimStyle.Render(indent+"      "+activity)
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
		if a.Presence != PresenceActive {
			continue
		}
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

	var header strings.Builder

	// Gradient title
	title := " Agent Monitor "
	if m.filterFavorites {
		title = " ★ Favorites "
	}
	header.WriteString(m.renderGradientTitle(title))
	header.WriteString("\n\n")

	// Build agent list lines
	var listLines []string
	if len(m.flatAgents) == 0 {
		listLines = append(listLines, normalStyle.Render("No agents found."))
		listLines = append(listLines, dimStyle.Render("Start an agent in tmux."))
	} else {
		agentIdx := 0
		for gi, g := range m.groups {
			// Group header (skip for empty name — flat mode)
			if g.Name != "" {
				var headerColor lipgloss.Color
				if g.Name == "Other" {
					headerColor = otherGroupColor
				} else {
					headerColor = groupHeaderColors[gi%len(groupHeaderColors)]
				}
				hdrStyle := lipgloss.NewStyle().
					Bold(true).
					Foreground(headerColor)
				listLines = append(listLines, hdrStyle.Render(fmt.Sprintf("┌ %s", g.Name)))
			}

			for _, item := range g.Items {
				if item.IsSubGroup {
					// Sub-group header
					listLines = append(listLines, dimStyle.Render(fmt.Sprintf("  ├ %s", item.SubGroup.Prefix)))
					for _, agent := range item.SubGroup.Agents {
						suffix := agent.Name[len(item.SubGroup.Prefix)+1:]
						rendered := m.renderAgentLine(agent, agentIdx, maxNameLen, suffix, "  ")
						for _, rl := range strings.Split(rendered, "\n") {
							listLines = append(listLines, rl)
						}
						agentIdx++
					}
				} else {
					rendered := m.renderAgentLine(item.Agent, agentIdx, maxNameLen, "", "")
					for _, rl := range strings.Split(rendered, "\n") {
						listLines = append(listLines, rl)
					}
					agentIdx++
				}
			}
		}
	}

	// Status summary
	var footer strings.Builder
	if len(m.flatAgents) > 0 {
		summary := m.renderStatusSummary()
		if summary != "" {
			footer.WriteString(dimStyle.Render("─") + " " + summary + "\n")
		}
	}
	if m.err != nil {
		footer.WriteString(statusError.Render(fmt.Sprintf("Error: %v", m.err)) + "\n")
	}

	// Compute available height for the agent list
	overhead := 7 // title(1) + blank(1) + borders(2) + help(3)
	if m.scrollOffset > 0 {
		overhead++ // "↑ more" line
	}
	availHeight := m.height - overhead
	if availHeight < 3 {
		availHeight = 3
	}

	// Slice the visible window
	endLine := m.scrollOffset + availHeight
	if endLine > len(listLines) {
		endLine = len(listLines)
	}
	visibleLines := listLines[m.scrollOffset:endLine]

	// Assemble panel content
	var content strings.Builder
	content.WriteString(header.String())
	if m.scrollOffset > 0 {
		content.WriteString(dimStyle.Render("  ↑ more") + "\n")
	}
	content.WriteString(strings.Join(visibleLines, "\n"))
	content.WriteString("\n")
	if endLine < len(listLines) {
		content.WriteString(dimStyle.Render("  ↓ more") + "\n")
	}
	content.WriteString(footer.String())

	// Apply panel border to agent list
	panelWidth := m.width - 2 // account for border
	if panelWidth < 20 {
		panelWidth = 20
	}
	agentPanel := panelStyle.Width(panelWidth).Render(content.String())

	// Help bar with its own border
	helpKeys := "j/k:nav  ⏎:attach  l:focus  ␣:fav  f:filter  a:activity  q:quit"
	helpText := helpStyle.Render(helpKeys) + "  " + dimStyle.Render(version)
	helpPanel := helpPanelStyle.Width(panelWidth).Render(helpText)

	return agentPanel + "\n" + helpPanel
}

// Key bindings
type keyMap struct {
	Up              key.Binding
	Down            key.Binding
	Attach          key.Binding
	FocusRight      key.Binding
	Refresh         key.Binding
	ToggleActivity  key.Binding
	ToggleFavorite  key.Binding
	FilterFavorites key.Binding
	Quit            key.Binding
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
	ToggleFavorite: key.NewBinding(
		key.WithKeys(" "),
	),
	FilterFavorites: key.NewBinding(
		key.WithKeys("f"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
	),
}

func main() {
	flag.Parse()

	// List mode: just print agents and exit
	if *listOnly {
		m := initialModel()
		m.agents = detectAgentsSync()
		m.buildGroups()
		agents := m.flatAgents
		if len(agents) == 0 {
			fmt.Println("No agents detected.")
			return
		}
		for _, agent := range agents {
			symbol := agent.Status.Symbol()
			fmt.Printf("%s %s %s (%s)\n", symbol, agent.Type.Badge(), agent.Name, agent.Status)
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

// detectPhantomSessions returns placeholder agents for sessions defined in
// groups.yaml that don't have a detected agent. Sessions with a running tmux
// session get PresenceNoAgent (selectable); others get PresenceNoSession.
func detectPhantomSessions(config Config, realAgents []Agent) []Agent {
	// Get all tmux sessions
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}")
	output, _ := cmd.Output()
	tmuxSessions := make(map[string]bool)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tmuxSessions[line] = true
		}
	}

	// Sessions that already have agents
	agentSessions := make(map[string]bool)
	for _, a := range realAgents {
		agentSessions[a.Session] = true
	}

	var phantoms []Agent
	for _, gc := range config.Groups {
		for _, s := range gc.Sessions {
			if strings.HasSuffix(s, "/*") {
				// Wildcard: check all tmux sessions matching the prefix
				prefix := strings.TrimSuffix(s, "/*")
				for sessionName := range tmuxSessions {
					if strings.HasPrefix(sessionName, prefix+"/") && !agentSessions[sessionName] {
						phantoms = append(phantoms, Agent{
							Name:     sessionName,
							Session:  sessionName,
							Type:     AgentUnknown,
							Status:   StatusUnknown,
							Presence: PresenceNoAgent,
						})
						agentSessions[sessionName] = true
					}
				}
			} else {
				if agentSessions[s] {
					continue
				}
				presence := PresenceNoSession
				if tmuxSessions[s] {
					presence = PresenceNoAgent
				}
				phantoms = append(phantoms, Agent{
					Name:     s,
					Session:  s,
					Type:     AgentUnknown,
					Status:   StatusUnknown,
					Presence: presence,
				})
				agentSessions[s] = true
			}
		}
	}
	return phantoms
}
