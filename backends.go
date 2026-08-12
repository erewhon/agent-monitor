package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// SourceTask is a task as reported by a backend. Project is the final swim-lane
// name (already remapped to the configured project). An empty Column means the
// backend has no opinion about status — the card keeps its local column.
type SourceTask struct {
	SourceID string
	Title    string
	Project  string
	Status   string // native status string, for logs
	Column   TaskColumn
	URL      string
}

// TaskSource is a pluggable project-management backend for the Kanban board.
type TaskSource interface {
	Name() string // unique routing key; also the card's Source field
	Kind() string // "nous" | "github" | "git-bug" — drives the display badge
	Fetch() ([]SourceTask, error)
	Push(t SourceTask, col TaskColumn) error // write a column change back
	Writable() bool
	PollInterval() time.Duration
}

// ── Config ───────────────────────────────────────────────────────────────

type backendsFile struct {
	Nous     *nousGlobalCfg `yaml:"nous"`
	Projects []projectCfg   `yaml:"projects"`
}

type nousGlobalCfg struct {
	URL       string `yaml:"url"`
	Notebook  string `yaml:"notebook"`
	Tag       string `yaml:"tag"`
	APIKey    string `yaml:"api_key"`
	Poll      int    `yaml:"poll_interval"`
	Writable  *bool  `yaml:"writable"`
	ImportAll *bool  `yaml:"import_all"`
}

type projectCfg struct {
	Name        string         `yaml:"name"`
	NousProject string         `yaml:"nous_project"`
	Backends    []backendEntry `yaml:"backends"`
}

// backendEntry is a single-key map: exactly one of the pointers is non-nil.
type backendEntry struct {
	Nous   *nousBackendCfg   `yaml:"nous"`
	GitHub *githubBackendCfg `yaml:"github"`
	GitBug *gitbugBackendCfg `yaml:"git-bug"`
}

type nousBackendCfg struct{} // marker: this project pulls from the global Nous

type githubBackendCfg struct {
	Host            string `yaml:"host"`
	Repo            string `yaml:"repo"` // "owner/repo"
	Token           string `yaml:"token"`
	TokenCmd        string `yaml:"token_cmd"`
	Writable        *bool  `yaml:"writable"`
	Poll            int    `yaml:"poll_interval"`
	ActiveLabel     string `yaml:"active_label"`
	NeedsInputLabel string `yaml:"needs_input_label"`
}

type gitbugBackendCfg struct {
	Repo        string `yaml:"repo"` // path to a git-bug-enabled repo
	Writable    *bool  `yaml:"writable"`
	Poll        int    `yaml:"poll_interval"`
	ActiveLabel string `yaml:"active_label"`
}

func backendsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agent-monitor", "backends.yaml")
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// loadBackendsConfig builds the full set of task sources. It reads
// backends.yaml; if absent, it falls back to the legacy nous.yaml so existing
// single-backend setups keep working untouched. Returns nil if nothing is
// configured.
func loadBackendsConfig() []TaskSource {
	var cfg backendsFile

	if path := backendsPath(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				recordBackendErr("backends.yaml", "config", err)
				return nil
			}
		}
	}

	// Back-compat: no backends.yaml → synthesize from the legacy nous.yaml.
	if cfg.Nous == nil && len(cfg.Projects) == 0 {
		legacy := loadNousConfig()
		if legacy == nil {
			return nil
		}
		importAll := true
		cfg.Nous = &nousGlobalCfg{
			URL:       legacy.URL,
			Notebook:  legacy.Notebook,
			Tag:       legacy.Tag,
			Poll:      legacy.PollInterval,
			ImportAll: &importAll,
		}
	}

	var sources []TaskSource

	// projectMap: Nous project name → configured group name (projects that
	// explicitly declare a nous backend). claimed: every Nous project name
	// owned by a projects[] entry, so import_all skips it (authoritative rule).
	projectMap := make(map[string]string)
	claimed := make(map[string]bool)

	for _, p := range cfg.Projects {
		if p.Name == "" {
			continue
		}
		nousName := p.NousProject
		if nousName == "" {
			nousName = p.Name
		}
		claimed[nousName] = true

		for _, be := range p.Backends {
			switch {
			case be.Nous != nil:
				projectMap[nousName] = p.Name
			case be.GitHub != nil:
				if s := newGithubSource(be.GitHub, p.Name); s != nil {
					sources = append(sources, s)
				}
			case be.GitBug != nil:
				if s := newGitbugSource(be.GitBug, p.Name); s != nil {
					sources = append(sources, s)
				}
			}
		}
	}

	// Single global Nous source: serves explicitly-declared projects (remapped
	// group names) plus, when import_all is on, every unclaimed Nous project.
	if cfg.Nous != nil {
		nc := &NousConfig{
			URL:          cfg.Nous.URL,
			Notebook:     cfg.Nous.Notebook,
			Tag:          cfg.Nous.Tag,
			APIKey:       cfg.Nous.APIKey,
			PollInterval: cfg.Nous.Poll,
		}
		if nc.APIKey == "" {
			nc.APIKey = os.Getenv("NOUS_API_KEY")
		}
		if nc.APIKey == "" {
			nc.APIKey = discoverNousAPIKey()
		}
		if nc.URL == "" {
			nc.URL = "http://localhost:7667"
		}
		if nc.Tag == "" {
			nc.Tag = "agent-monitor"
		}
		if nc.PollInterval <= 0 {
			nc.PollInterval = 30
		}
		sources = append(sources, &nousSource{
			client:     newNousClient(nc),
			tag:        nc.Tag,
			writable:   boolOr(cfg.Nous.Writable, true),
			poll:       time.Duration(nc.PollInterval) * time.Second,
			importAll:  boolOr(cfg.Nous.ImportAll, true),
			projectMap: projectMap,
			claimed:    claimed,
			pageTags:   make(map[string][]string),
		})
	} else if len(projectMap) > 0 {
		recordBackendErr("nous", "config",
			fmt.Errorf("a project declares a nous backend but no global `nous:` block is configured"))
	}

	return sources
}

// ── Nous source ──────────────────────────────────────────────────────────

type nousSource struct {
	client     *NousClient
	tag        string
	writable   bool
	poll       time.Duration
	importAll  bool
	projectMap map[string]string // Nous project → configured group
	claimed    map[string]bool   // Nous projects owned by an explicit entry

	mu       sync.Mutex
	pageTags map[string][]string // page id → tags, cached from Fetch for Push
}

func (s *nousSource) Name() string                { return "nous" }
func (s *nousSource) Kind() string                { return "nous" }
func (s *nousSource) Writable() bool              { return s.writable }
func (s *nousSource) PollInterval() time.Duration { return s.poll }

// resolveGroup decides the swim-lane for a Nous project, and whether to include
// it at all. Explicitly-declared projects are remapped; claimed-but-not-declared
// projects are excluded (they moved off Nous); the rest depend on import_all.
func (s *nousSource) resolveGroup(nousProject string) (string, bool) {
	if g, ok := s.projectMap[nousProject]; ok {
		return g, true
	}
	if s.claimed[nousProject] {
		return "", false
	}
	if s.importAll {
		return nousProject, true
	}
	return "", false
}

func (s *nousSource) Fetch() ([]SourceTask, error) {
	pages, err := s.client.listPages()
	if err != nil {
		return nil, err
	}
	rows, _ := s.client.getTaskRows() // best-effort status/project enrichment
	rowByName := make(map[string]nousTaskRow, len(rows))
	for _, r := range rows {
		rowByName[r.Task] = r
	}

	tagsByPage := make(map[string][]string)
	var out []SourceTask
	for _, p := range pages {
		if !hasTag(p, s.tag) {
			continue
		}
		name := strings.TrimPrefix(p.Title, "Task: ")
		row, ok := rowByName[name]
		if !ok {
			row, ok = rowByName[p.Title]
		}

		nousProject := ""
		status := ""
		col := TaskColumn("") // no column opinion unless a DB row exists
		if ok {
			nousProject = row.Project
			status = row.Status
			col = nousStatusToColumn(row.Status)
		}
		if nousProject == "" {
			nousProject = inferProjectFromTags(p.Tags)
		}

		group, include := s.resolveGroup(nousProject)
		if !include {
			continue
		}
		tagsByPage[p.ID] = p.Tags
		out = append(out, SourceTask{
			SourceID: p.ID,
			Title:    p.Title,
			Project:  group,
			Status:   status,
			Column:   col,
		})
	}

	s.mu.Lock()
	s.pageTags = tagsByPage
	s.mu.Unlock()
	return out, nil
}

func (s *nousSource) tagsFor(pageID string) []string {
	s.mu.Lock()
	tags, ok := s.pageTags[pageID]
	s.mu.Unlock()
	if ok {
		return tags
	}
	// Cache miss (e.g. Push before first Fetch): refresh from the API so we
	// never clobber existing tags with a status-only set.
	if pages, err := s.client.listPages(); err == nil {
		for _, p := range pages {
			if p.ID == pageID {
				return p.Tags
			}
		}
	}
	return nil
}

func (s *nousSource) Push(t SourceTask, col TaskColumn) error {
	tags := s.tagsFor(t.SourceID)
	newTags := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		if tag != "active" && tag != "done" && tag != "needs-input" {
			newTags = append(newTags, tag)
		}
	}
	switch col {
	case ColumnActive:
		newTags = append(newTags, "active")
	case ColumnNeedsInput:
		newTags = append(newTags, "needs-input")
	case ColumnDone:
		newTags = append(newTags, "done")
	}
	if err := s.client.updateTags(t.SourceID, newTags); err != nil {
		return err
	}
	s.client.appendToPage(t.SourceID, fmt.Sprintf("\n---\n*%s — moved to %s*\n",
		time.Now().Format("2006-01-02 15:04"), col))
	return nil
}

// ── Backend health & logging ─────────────────────────────────────────────

// tuiActive is set once the Bubble Tea program owns the terminal. While it is
// set, nothing may write to stderr: a stray Fprintf punches a hole through the
// rendered frame and stays there until the next full redraw.
var tuiActive bool

// backendIssue is the current failure for one backend. Sync runs in background
// goroutines, so failures are recorded here and surfaced by the TUI footer and
// the log file rather than printed.
type backendIssue struct {
	Source string
	Op     string // "fetch" | "push" | "config"
	Err    string
	Since  time.Time // first occurrence of this message
	Count  int       // consecutive occurrences
}

var backendHealth = struct {
	mu     sync.Mutex
	issues map[string]backendIssue // "source|op" → latest issue
}{issues: make(map[string]backendIssue)}

// healthKey scopes an issue to one operation, so a healthy fetch doesn't clear a
// standing push failure (writes can be rejected while reads still work).
func healthKey(source, op string) string { return source + "|" + op }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// recordBackendErr notes a failure. Repeats of the same message are counted but
// only logged on first sight and every 20th repeat, so a backend that is down
// for hours doesn't fill the log with one line per poll.
func recordBackendErr(source, op string, err error) {
	msg := err.Error()
	key := healthKey(source, op)
	backendHealth.mu.Lock()
	prev, existed := backendHealth.issues[key]
	issue := backendIssue{Source: source, Op: op, Err: msg, Since: time.Now(), Count: 1}
	if existed && prev.Err == msg {
		issue.Since = prev.Since
		issue.Count = prev.Count + 1
	}
	backendHealth.issues[key] = issue
	backendHealth.mu.Unlock()

	if issue.Count == 1 || issue.Count%20 == 0 {
		suffix := ""
		if issue.Count > 1 {
			suffix = fmt.Sprintf(" (×%d since %s)", issue.Count, issue.Since.Format("15:04"))
		}
		backendLogf("%s %s failed: %s%s", source, op, msg, suffix)
	}
}

// clearBackendErr marks one operation on a source healthy again, logging the
// recovery if it was previously failing.
func clearBackendErr(source, op string) {
	key := healthKey(source, op)
	backendHealth.mu.Lock()
	prev, existed := backendHealth.issues[key]
	delete(backendHealth.issues, key)
	backendHealth.mu.Unlock()
	if existed {
		backendLogf("%s %s recovered after %d failed attempt%s", source, op, prev.Count, plural(prev.Count))
	}
}

// backendIssues returns the current failures, ordered by source then operation
// for a stable display.
func backendIssues() []backendIssue {
	backendHealth.mu.Lock()
	out := make([]backendIssue, 0, len(backendHealth.issues))
	for _, iss := range backendHealth.issues {
		out = append(out, iss)
	}
	backendHealth.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Op < out[j].Op
	})
	return out
}

// logFilePath is where background failures go while the TUI owns the terminal.
func logFilePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "agent-monitor", "agent-monitor.log")
}

var logMu sync.Mutex

// backendLogf appends a timestamped line to the log file. Without a TUI
// (--web-only, --list) it also echoes to stderr, where a service manager
// expects it.
func backendLogf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !tuiActive {
		fmt.Fprintf(os.Stderr, "agent-monitor: %s\n", line)
	}
	path := logFilePath()
	if path == "" {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// Keep the log bounded: one rollover, no external rotation needed.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
}

// ── Generic sync loop ────────────────────────────────────────────────────

// startBackendSyncLoop runs one goroutine per source, each reconciling its
// backend against the shared TaskStore on its own poll interval.
func startBackendSyncLoop(sources []TaskSource, tasks *TaskStore, hub *SSEHub) {
	for _, src := range sources {
		go runSource(src, tasks, hub)
	}
}

func runSource(src TaskSource, tasks *TaskStore, hub *SSEHub) {
	prevColumns := make(map[int]TaskColumn)
	interval := src.PollInterval()
	if interval <= 0 {
		interval = 60 * time.Second
	}
	for {
		syncSourceOnce(src, tasks, hub, prevColumns)
		time.Sleep(interval)
	}
}

func syncSourceOnce(src TaskSource, tasks *TaskStore, hub *SSEHub, prevColumns map[int]TaskColumn) {
	// Board → backend FIRST: push locally-originated column changes before the
	// authoritative pull below, otherwise a just-moved card would be reverted to
	// the backend's stale state before the push ever fires. New/unseen tasks are
	// only seeded here (no push), so startup never emits a spurious write.
	for _, t := range tasks.List(true) {
		if t.Source != src.Name() || t.Column == ColumnArchived {
			continue
		}
		prev, known := prevColumns[t.ID]
		if !known {
			prevColumns[t.ID] = t.Column
			continue
		}
		if t.Column == prev {
			continue
		}
		prevColumns[t.ID] = t.Column
		if src.Writable() {
			if err := src.Push(SourceTask{SourceID: t.SourceID, Title: t.Title, Project: t.Group}, t.Column); err != nil {
				recordBackendErr(src.Name(), "push", fmt.Errorf("%s: %w", t.SourceID, err))
			} else {
				clearBackendErr(src.Name(), "push")
			}
		}
	}

	// Backend → board: pull current state and reconcile.
	sts, err := src.Fetch()
	if err != nil {
		recordBackendErr(src.Name(), "fetch", err)
		return
	}
	clearBackendErr(src.Name(), "fetch")
	for _, st := range sts {
		id, backendMovedColumn := upsertSourceTask(tasks, hub, src.Name(), st)
		if backendMovedColumn {
			// The backend drove this column change; record it so the next
			// board→backend pass doesn't echo it back as a local move.
			prevColumns[id] = st.Column
		}
	}
}

// upsertSourceTask creates or updates the local card for a SourceTask. Returns
// the task id and whether the backend changed its column this pass.
func upsertSourceTask(tasks *TaskStore, hub *SSEHub, source string, st SourceTask) (int, bool) {
	existing, ok := tasks.GetBySource(source, st.SourceID)
	if !ok {
		col := st.Column
		if col == "" {
			col = ColumnBacklog
		}
		created := tasks.CreateSourced(source, st.SourceID, st.Title, st.Project, st.URL, col)
		broadcastTask(hub, "task:created", created)
		return created.ID, false
	}

	patch := taskPatchRequest{}
	changed := false
	colChanged := false
	if st.Title != "" && st.Title != existing.Title {
		title := st.Title
		patch.Title = &title
		changed = true
	}
	if st.Project != existing.Group {
		group := st.Project
		patch.Group = &group
		changed = true
	}
	if st.URL != existing.URL {
		url := st.URL
		patch.URL = &url
		changed = true
	}
	if st.Column != "" && st.Column != existing.Column {
		col := st.Column
		patch.Column = &col
		changed = true
		colChanged = true
	}
	if changed {
		if updated, ok := tasks.Update(existing.ID, patch); ok {
			broadcastTask(hub, "task:updated", updated)
		}
	}
	return existing.ID, colChanged
}

func broadcastTask(hub *SSEHub, event string, t Task) {
	if hub == nil {
		return
	}
	data, _ := json.Marshal(t)
	hub.Broadcast(SSEEvent{Type: event, Data: data})
}
