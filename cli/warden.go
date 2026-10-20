package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Warden rows: contract-work VMs watched through the warden forced-command
// gate (github.com/erewhon … smithy/warden). The only thing agent-monitor
// ever sends is the verb `status`; the only thing it ever receives is the
// Status document — numbers, enums, timestamps and three opaque labels — so a
// warden row shows liveness, state and counters, never content. Selecting a
// row runs the warden's configured `attach` command in the content pane
// (an ssh session, when the operator may enter the VM) or, without one, a
// read-only status panel.

// AgentWarden is the row type for a warden-watched VM.
const AgentWarden AgentType = "warden"

// wardenCfg is one entry of ~/.config/agent-monitor/wardens.yaml.
type wardenCfg struct {
	Label  string `yaml:"label"` // the VM's label, as it reports itself
	Host   string `yaml:"host"`
	User   string `yaml:"user"` // default "warden"
	Key    string `yaml:"key"`
	Port   int    `yaml:"port"`   // default 22
	Poll   int    `yaml:"poll_s"` // default 30
	Attach string `yaml:"attach"` // shell command for the content pane; empty = status panel
	// StaleMin is how old the heartbeat may be before the row reads as Error.
	StaleMin int `yaml:"stale_min"` // default 10
	// NotifyNtfy opts a warden into ntfy pushes; off by default because the
	// warden coordinator in meta already pages over Signal.
	NotifyNtfy *bool `yaml:"notify_ntfy"`
}

func (c wardenCfg) user() string {
	if c.User == "" {
		return "warden"
	}
	return c.User
}

func (c wardenCfg) poll() time.Duration {
	if c.Poll <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.Poll) * time.Second
}

func (c wardenCfg) stale() time.Duration {
	if c.StaleMin <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(c.StaleMin) * time.Minute
}

type wardensFile struct {
	Wardens []wardenCfg `yaml:"wardens"`
}

func wardensPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agent-monitor", "wardens.yaml")
}

// loadWardensConfig reads wardens.yaml. Missing file: no wardens. A malformed
// file is reported through the backend health registry, never stderr.
func loadWardensConfig() []wardenCfg {
	path := wardensPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	cfgs, err := parseWardensConfig(data)
	if err != nil {
		recordBackendErr("wardens.yaml", "config", err)
		return nil
	}
	return cfgs
}

func parseWardensConfig(data []byte) ([]wardenCfg, error) {
	var f wardensFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []wardenCfg
	for i, c := range f.Wardens {
		if c.Label == "" || c.Host == "" {
			return nil, fmt.Errorf("wardens[%d]: label and host are required", i)
		}
		if seen[c.Label] {
			return nil, fmt.Errorf("wardens: duplicate label %q", c.Label)
		}
		seen[c.Label] = true
		c.Key = expandHome(c.Key)
		out = append(out, c)
	}
	return out, nil
}

// ── The Status document ──────────────────────────────────────────────────

// wardenCurrent mirrors warden.models.Current. Pointers are nullable fields:
// the VM's disclosure tier, or a loop without a queue, serves them as null.
type wardenCurrent struct {
	ID        *string    `json:"id"`
	Title     *string    `json:"title"`
	State     string     `json:"state"` // running | blocked | done | idle
	StartedAt *time.Time `json:"started_at"`
}

// wardenStatus mirrors warden.models.Status.
type wardenStatus struct {
	VM                string         `json:"vm"`
	QueueDepth        *int           `json:"queue_depth"`
	Current           *wardenCurrent `json:"current"`
	HeartbeatAt       time.Time      `json:"heartbeat_at"`
	CommitsSinceStart *int           `json:"commits_since_start"`
	Tests             map[string]int `json:"tests"`
	ReasonCode        string         `json:"reason_code"` // OK | NO_TASKS | TESTS_FAILING | NEEDS_HUMAN
	LastEventAt       *time.Time     `json:"last_event_at"`
	Paused            bool           `json:"paused"`
}

func parseWardenStatus(data []byte) (*wardenStatus, error) {
	var s wardenStatus
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("bad document")
	}
	if s.VM == "" || s.HeartbeatAt.IsZero() || s.ReasonCode == "" {
		return nil, fmt.Errorf("bad document")
	}
	return &s, nil
}

// wardenSample is the latest poll of one warden.
type wardenSample struct {
	cfg wardenCfg
	doc *wardenStatus
	err error     // nil when doc is set
	at  time.Time // when the poll finished
}

// ── Mapping (pure) ───────────────────────────────────────────────────────

// wardenRow maps one sample to a sidebar row. Only the document's enums and
// numbers reach the row: the name is the configured label, LastLine is built
// from counters, and the attach command is the operator's own.
func wardenRow(s wardenSample, now time.Time) Agent {
	a := Agent{
		Name:      s.cfg.Label,
		Session:   s.cfg.Label,
		Type:      AgentWarden,
		Presence:  PresenceActive,
		Host:      s.cfg.Host,
		Attach:    s.cfg.Attach,
		UpdatedAt: s.at,
	}
	if s.at.IsZero() {
		a.Status = StatusUnknown
		a.LastLine = "polling"
		return a
	}
	if s.err != nil || s.doc == nil {
		a.Status = StatusError
		a.LastLine = "unreachable"
		if s.err != nil {
			a.LastLine = "unreachable: " + s.err.Error()
		}
		return a
	}
	d := s.doc
	if d.VM != s.cfg.Label {
		a.Status = StatusError
		a.LastLine = "vm label mismatch"
		return a
	}
	age := now.Sub(d.HeartbeatAt)
	if age > s.cfg.stale() {
		a.Status = StatusError
		a.LastLine = "stale: " + wardenLastLine(d, now)
		return a
	}
	state := ""
	if d.Current != nil {
		state = d.Current.State
	}
	switch {
	case d.Paused:
		a.Status = StatusIdle
		a.LastLine = "paused · " + wardenLastLine(d, now)
		return a
	case state == "blocked" || d.ReasonCode == "NEEDS_HUMAN" || d.ReasonCode == "TESTS_FAILING":
		a.Status = StatusWaiting
		if d.ReasonCode == "TESTS_FAILING" {
			a.Wait = WaitInput
		} else {
			a.Wait = WaitApproval
		}
	case state == "running":
		a.Status = StatusRunning
	default: // idle, done, or no current task
		a.Status = StatusIdle
	}
	a.LastLine = wardenLastLine(d, now)
	return a
}

// wardenLastLine renders the counters: "commits 6 · hb 40s · OK · q 0 · t 40/0".
// Null fields print as "?".
func wardenLastLine(d *wardenStatus, now time.Time) string {
	parts := []string{
		"commits " + intOrQ(d.CommitsSinceStart),
		"hb " + shortAge(now.Sub(d.HeartbeatAt)),
		d.ReasonCode,
	}
	if d.QueueDepth != nil {
		parts = append(parts, fmt.Sprintf("q %d", *d.QueueDepth))
	}
	if d.Tests != nil {
		parts = append(parts, fmt.Sprintf("t %d/%d", d.Tests["pass"], d.Tests["fail"]))
	}
	return strings.Join(parts, " · ")
}

func intOrQ(p *int) string {
	if p == nil {
		return "?"
	}
	return fmt.Sprint(*p)
}

func shortAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// ── Polling ──────────────────────────────────────────────────────────────

// wardenSSHArgs is the hardened call: no user or system config (under a
// systemd user service root-owned /etc/ssh files can read as nobody's and
// make every host look unreachable), batch mode, this key only, no
// forwarding, no pty. The verb is the whole command.
func wardenSSHArgs(c wardenCfg, verb string) []string {
	args := []string{
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=10",
		"-T",
	}
	if c.Key != "" {
		args = append(args, "-i", c.Key)
	}
	if c.Port > 0 {
		args = append(args, "-p", fmt.Sprint(c.Port))
	}
	return append(args, c.user()+"@"+c.Host, verb)
}

// pollWarden runs one `status` call. Errors carry no VM text: ssh's own
// stderr is dropped, only the exit class is kept.
func pollWarden(ctx context.Context, c wardenCfg) (*wardenStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", wardenSSHArgs(c, "status")...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("timeout")
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("ssh exit %d", ee.ExitCode())
		}
		return nil, fmt.Errorf("ssh failed")
	}
	return parseWardenStatus(out)
}

// WardenStore holds the latest sample per warden; one goroutine per warden
// refreshes it. Readers get rows mapped at read time so staleness is judged
// against the current clock, not the poll's.
type WardenStore struct {
	mu      sync.RWMutex
	samples map[string]wardenSample
	order   []string
}

func newWardenStore(cfgs []wardenCfg) *WardenStore {
	ws := &WardenStore{samples: make(map[string]wardenSample, len(cfgs))}
	for _, c := range cfgs {
		ws.samples[c.Label] = wardenSample{cfg: c}
		ws.order = append(ws.order, c.Label)
	}
	return ws
}

func (ws *WardenStore) set(label string, doc *wardenStatus, err error, at time.Time) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s := ws.samples[label]
	s.doc, s.err, s.at = doc, err, at
	ws.samples[label] = s
}

// Agents returns one row per configured warden, in config order.
func (ws *WardenStore) Agents(now time.Time) []Agent {
	if ws == nil {
		return nil
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := make([]Agent, 0, len(ws.order))
	for _, label := range ws.order {
		out = append(out, wardenRow(ws.samples[label], now))
	}
	return out
}

// Config returns the configuration for a label, if it is a warden.
func (ws *WardenStore) Config(label string) (wardenCfg, bool) {
	if ws == nil {
		return wardenCfg{}, false
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	s, ok := ws.samples[label]
	return s.cfg, ok
}

// NtfyEnabled reports whether a warden row may push to ntfy (off by default).
func (ws *WardenStore) NtfyEnabled(label string) bool {
	c, ok := ws.Config(label)
	return ok && boolOr(c.NotifyNtfy, false)
}

// Start launches the pollers. They stop when ctx is cancelled.
func (ws *WardenStore) Start(ctx context.Context) {
	for _, label := range ws.order {
		c := ws.samples[label].cfg
		go func(c wardenCfg) {
			for {
				doc, err := pollWarden(ctx, c)
				if ctx.Err() != nil {
					return
				}
				ws.set(c.Label, doc, err, time.Now())
				if err != nil {
					recordBackendErr("warden:"+c.Label, "status", err)
				} else {
					clearBackendErr("warden:"+c.Label, "status")
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(c.poll()):
				}
			}
		}(c)
	}
}

// PollOnce refreshes every warden synchronously (for --list and the panel).
func (ws *WardenStore) PollOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, label := range ws.order {
		c := ws.samples[label].cfg
		wg.Add(1)
		go func(c wardenCfg) {
			defer wg.Done()
			doc, err := pollWarden(ctx, c)
			ws.set(c.Label, doc, err, time.Now())
		}(c)
	}
	wg.Wait()
}

// ── Attach ───────────────────────────────────────────────────────────────

// attachCommand is the shell fragment respawn-pane runs when a row is
// selected: the row's own attach command when it has one, the status panel
// for a warden without one, else the local tmux attach.
func attachCommand(agent Agent) string {
	if agent.Attach != "" {
		return agent.Attach
	}
	if agent.Type == AgentWarden {
		return selfShell("warden-status", agent.Session)
	}
	attachTarget := agent.Target()
	if agent.Presence == PresenceNoAgent {
		attachTarget = agent.Session
	}
	return fmt.Sprintf("unset TMUX; exec tmux attach-session -t '%s'", attachTarget)
}

// ── Status panel ─────────────────────────────────────────────────────────

// coordinatorState is the slice of meta's work-watcher state.json the panel
// shows: what the paging coordinator last did about this VM.
type coordinatorState struct {
	Commits          *int       `json:"commits"`
	CommitsChangedAt *time.Time `json:"commits_changed_at"`
	LastNudgeAt      *time.Time `json:"last_nudge_at"`
	LastPageAt       *time.Time `json:"last_page_at"`
	LastPageCode     *string    `json:"last_page_code"`
	Failures         int        `json:"failures"`
	LastSeenAt       *time.Time `json:"last_seen_at"`
}

func loadCoordinatorState(label string) *coordinatorState {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".local", "share", "work-watcher", "state.json"))
	if err != nil {
		return nil
	}
	var file struct {
		Warden map[string]coordinatorState `json:"warden"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	st, ok := file.Warden[label]
	if !ok {
		return nil
	}
	return &st
}

// renderWardenPanel is the read-only view for a warden without an attach
// command. Everything on it is from the Status document or the coordinator's
// own memory; nothing from inside the VM.
func renderWardenPanel(s wardenSample, co *coordinatorState, now time.Time) string {
	var b strings.Builder
	row := wardenRow(s, now)
	fmt.Fprintf(&b, "warden %s  (%s)\n", s.cfg.Label, s.cfg.Host)
	status := row.Status.String()
	symbol := row.Status.Symbol()
	if w := row.WaitReasonOrNone(); w != WaitNone {
		status += ":" + w.String()
		symbol = w.Symbol()
	}
	fmt.Fprintf(&b, "  state      %s %s\n", symbol, status)
	if s.at.IsZero() {
		b.WriteString("  polled     never\n")
	} else {
		fmt.Fprintf(&b, "  polled     %s ago\n", shortAge(now.Sub(s.at)))
	}
	if s.err != nil {
		fmt.Fprintf(&b, "  error      %s\n", s.err.Error())
	}
	if d := s.doc; d != nil {
		fmt.Fprintf(&b, "  heartbeat  %s ago\n", shortAge(now.Sub(d.HeartbeatAt)))
		if d.Current != nil {
			fmt.Fprintf(&b, "  current    %s", d.Current.State)
			if d.Current.ID != nil {
				fmt.Fprintf(&b, "  id %s", *d.Current.ID)
			}
			if d.Current.Title != nil {
				fmt.Fprintf(&b, "  %q", *d.Current.Title)
			}
			if d.Current.StartedAt != nil {
				fmt.Fprintf(&b, "  since %s", shortAge(now.Sub(*d.Current.StartedAt)))
			}
			b.WriteString("\n")
		} else {
			b.WriteString("  current    none\n")
		}
		fmt.Fprintf(&b, "  reason     %s\n", d.ReasonCode)
		fmt.Fprintf(&b, "  commits    %s since start\n", intOrQ(d.CommitsSinceStart))
		fmt.Fprintf(&b, "  queue      %s\n", intOrQ(d.QueueDepth))
		if d.Tests != nil {
			fmt.Fprintf(&b, "  tests      %d pass / %d fail\n", d.Tests["pass"], d.Tests["fail"])
		}
		if d.LastEventAt != nil {
			fmt.Fprintf(&b, "  last event %s ago\n", shortAge(now.Sub(*d.LastEventAt)))
		}
		fmt.Fprintf(&b, "  paused     %v\n", d.Paused)
	}
	if co != nil {
		b.WriteString("\ncoordinator (meta warden)\n")
		if co.LastSeenAt != nil {
			fmt.Fprintf(&b, "  last seen  %s ago\n", shortAge(now.Sub(*co.LastSeenAt)))
		}
		if co.LastNudgeAt != nil {
			fmt.Fprintf(&b, "  last nudge %s ago\n", shortAge(now.Sub(*co.LastNudgeAt)))
		}
		if co.LastPageAt != nil {
			code := ""
			if co.LastPageCode != nil {
				code = " " + *co.LastPageCode
			}
			fmt.Fprintf(&b, "  last page  %s ago%s\n", shortAge(now.Sub(*co.LastPageAt)), code)
		}
		if co.Failures > 0 {
			fmt.Fprintf(&b, "  failures   %d in a row\n", co.Failures)
		}
	}
	return b.String()
}

// runWardenStatus is the `warden-status <label>` subcommand: the panel the
// content pane shows for a warden without an attach command. It refreshes
// on the warden's poll interval until the pane is respawned.
func runWardenStatus(ctx context.Context, label string, once bool) error {
	var cfg wardenCfg
	found := false
	for _, c := range loadWardensConfig() {
		if c.Label == label {
			cfg, found = c, true
			break
		}
	}
	if !found {
		return fmt.Errorf("no warden %q in %s", label, wardensPath())
	}
	for {
		doc, err := pollWarden(ctx, cfg)
		now := time.Now()
		s := wardenSample{cfg: cfg, doc: doc, err: err, at: now}
		panel := renderWardenPanel(s, loadCoordinatorState(label), now)
		if once {
			fmt.Print(panel)
			return nil
		}
		fmt.Print("\033[H\033[2J" + panel + "\n(refreshes every " + cfg.poll().String() + ")\n")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.poll()):
		}
	}
}
