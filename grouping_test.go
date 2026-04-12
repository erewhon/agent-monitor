package main

import (
	"testing"
	"time"
)

// mkAgent builds a minimal active agent for grouping tests.
func mkAgent(name string, status AgentStatus) Agent {
	return Agent{Name: name, Session: name, Type: AgentClaude, Status: status, Presence: PresenceActive}
}

// groupNames returns the display-ordered names of a model's groups.
func groupNames(m *Model) []string {
	var out []string
	for _, g := range m.groups {
		out = append(out, g.Name)
	}
	return out
}

func TestBuildStatusGroupsOrderAndBucketing(t *testing.T) {
	m := &Model{
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{"done1": time.Now()}, // recently active => "Done"
		agents: []Agent{
			mkAgent("idle1", StatusIdle),     // no lastActiveAt => "Idle"
			mkAgent("run1", StatusRunning),   // "Running"
			mkAgent("wait1", StatusWaiting),  // "Waiting"
			mkAgent("done1", StatusIdle),     // recent => "Done"
			mkAgent("plan1", StatusPlanning), // "Planning"
			mkAgent("err1", StatusError),     // "Error"
		},
	}
	m.buildGroups()

	// Buckets must appear in attention-priority order, skipping empty ones.
	want := []string{"Waiting", "Error", "Running", "Planning", "Done", "Idle"}
	got := groupNames(m)
	if len(got) != len(want) {
		t.Fatalf("group count = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Every live agent should appear exactly once across flatAgents.
	if len(m.flatAgents) != 6 {
		t.Errorf("flatAgents count = %d, want 6", len(m.flatAgents))
	}
}

func TestStatusGroupsPutPhantomsInOffline(t *testing.T) {
	m := &Model{
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{},
		agents: []Agent{
			mkAgent("live", StatusRunning),
			{Name: "phantom", Session: "phantom", Status: StatusIdle, Presence: PresenceNoSession},
		},
	}
	m.buildGroups()

	// Live agents bucket by status; non-active sessions collect in "Offline"
	// at the bottom so nothing silently vanishes in the status lens.
	want := []string{"Running", "Offline"}
	if got := groupNames(m); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	// The live agent leads flatAgents; the phantom is present but non-selectable.
	if len(m.flatAgents) != 2 || m.flatAgents[0].Name != "live" {
		t.Errorf("flatAgents = %v, want [live phantom]", m.flatAgents)
	}
}

func TestCollapseExcludesFromFlatButKeepsGroupAndDisplay(t *testing.T) {
	m := &Model{
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{},
		collapsed:     map[string]bool{"Running": true},
		agents: []Agent{
			mkAgent("wait1", StatusWaiting),
			mkAgent("run1", StatusRunning),
			mkAgent("run2", StatusRunning),
		},
	}
	m.buildGroups()

	// The collapsed group's header is still present...
	names := groupNames(m)
	if len(names) != 2 || names[0] != "Waiting" || names[1] != "Running" {
		t.Fatalf("groups = %v, want [Waiting Running]", names)
	}
	// ...but its agents are excluded from the navigable flat list.
	if len(m.flatAgents) != 1 {
		t.Errorf("flatAgents = %d, want 1 (only Waiting visible)", len(m.flatAgents))
	}
	// displayAgents ignores collapse — the API must see all three.
	if got := len(m.displayAgents()); got != 3 {
		t.Errorf("displayAgents = %d, want 3 (collapse-independent)", got)
	}
}

func TestCursorGroupNameAndFlatRange(t *testing.T) {
	m := &Model{
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{},
		agents: []Agent{
			mkAgent("wait1", StatusWaiting), // flat idx 0 — Waiting
			mkAgent("run1", StatusRunning),  // flat idx 1 — Running
			mkAgent("run2", StatusRunning),  // flat idx 2 — Running
		},
	}
	m.buildGroups()

	cases := map[int]string{0: "Waiting", 1: "Running", 2: "Running"}
	for cursor, want := range cases {
		m.cursor = cursor
		if got := m.cursorGroupName(); got != want {
			t.Errorf("cursorGroupName(cursor=%d) = %q, want %q", cursor, got, want)
		}
	}

	if start, count := m.groupFlatRange("Running"); start != 1 || count != 2 {
		t.Errorf("groupFlatRange(Running) = (%d,%d), want (1,2)", start, count)
	}
	if start, count := m.groupFlatRange("Waiting"); start != 0 || count != 1 {
		t.Errorf("groupFlatRange(Waiting) = (%d,%d), want (0,1)", start, count)
	}
	if start, _ := m.groupFlatRange("Nonexistent"); start != -1 {
		t.Errorf("groupFlatRange(Nonexistent) start = %d, want -1", start)
	}
}

func TestCollapsedGroupFlatRangePointsAtNextGroup(t *testing.T) {
	// When a group collapses, its start index should equal where the next
	// group's agents begin (it contributes zero visible rows).
	m := &Model{
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{},
		collapsed:     map[string]bool{"Waiting": true},
		agents: []Agent{
			mkAgent("wait1", StatusWaiting),
			mkAgent("run1", StatusRunning),
		},
	}
	m.buildGroups()
	if start, count := m.groupFlatRange("Waiting"); start != 0 || count != 0 {
		t.Errorf("collapsed Waiting range = (%d,%d), want (0,0)", start, count)
	}
	if start, count := m.groupFlatRange("Running"); start != 0 || count != 1 {
		t.Errorf("Running range = (%d,%d), want (0,1)", start, count)
	}
}

func TestConfigGroupCollapse(t *testing.T) {
	m := &Model{
		config: Config{Groups: []GroupConfig{
			{Name: "Alpha", Sessions: []string{"a1"}},
			{Name: "Beta", Sessions: []string{"b1"}},
		}},
		collapsed:    map[string]bool{"Alpha": true},
		lastActiveAt: map[string]time.Time{},
		agents: []Agent{
			mkAgent("a1", StatusRunning),
			mkAgent("b1", StatusRunning),
		},
	}
	m.buildGroups()
	if names := groupNames(m); len(names) != 2 {
		t.Fatalf("groups = %v, want 2", names)
	}
	if len(m.flatAgents) != 1 || m.flatAgents[0].Name != "b1" {
		t.Errorf("flatAgents = %v, want [b1] (Alpha collapsed)", m.flatAgents)
	}
	if len(m.displayAgents()) != 2 {
		t.Errorf("displayAgents = %d, want 2", len(m.displayAgents()))
	}
}

func TestIsCollapsedFlatGroupNeverCollapses(t *testing.T) {
	m := &Model{collapsed: map[string]bool{"": true}}
	if m.isCollapsed("") {
		t.Error("flat group (empty name) must never be collapsed")
	}
}

// TestViewRendersStatusGroupsWithCollapse exercises the render path (header
// chevron + skip-collapsed-items) to catch panics; it asserts the collapsed
// group's header appears while its agent rows do not.
func TestViewRendersStatusGroupsWithCollapse(t *testing.T) {
	m := &Model{
		width:         80,
		height:        24,
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{},
		favorites:     map[string]bool{},
		collapsed:     map[string]bool{"Running": true},
		agents: []Agent{
			mkAgent("wait1", StatusWaiting),
			mkAgent("run1", StatusRunning),
		},
	}
	m.buildGroups()
	out := stripAnsi(m.View())
	if out == "" {
		t.Fatal("View() returned empty output")
	}
	if !contains(out, "▸ Running (1)") {
		t.Errorf("collapsed Running header missing from view:\n%s", out)
	}
	if contains(out, "run1") {
		t.Errorf("collapsed group's agent row should be hidden, but found run1:\n%s", out)
	}
	if !contains(out, "wait1") {
		t.Errorf("expanded Waiting group's agent row missing:\n%s", out)
	}
}

// TestViewShowsHeaderWhenAllGroupsCollapsed guards the regression where a sole
// collapsed group left flatAgents empty and the View fell back to "No agents
// found", hiding the header the user just folded.
func TestViewShowsHeaderWhenAllGroupsCollapsed(t *testing.T) {
	m := &Model{
		width:         80,
		height:        24,
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{},
		favorites:     map[string]bool{},
		collapsed:     map[string]bool{"Running": true},
		agents:        []Agent{mkAgent("run1", StatusRunning)},
	}
	m.buildGroups()
	if len(m.flatAgents) != 0 {
		t.Fatalf("precondition: flatAgents should be empty, got %d", len(m.flatAgents))
	}
	out := stripAnsi(m.View())
	if contains(out, "No agents found") {
		t.Errorf("view showed empty message despite a collapsed group:\n%s", out)
	}
	if !contains(out, "▸ Running (1)") {
		t.Errorf("collapsed header missing:\n%s", out)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
