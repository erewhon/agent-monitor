package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestTruncateCells(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits exactly", "abcdef", 6, "abcdef"},
		{"shorter than budget", "abc", 10, "abc"},
		{"trimmed with ellipsis", "abcdefgh", 5, "abcd…"},
		{"single cell", "abcdef", 1, "…"},
		{"zero budget", "abcdef", 0, ""},
		{"negative budget", "abcdef", -3, ""},
		// Wide runes cost two cells each: three of them don't fit in four.
		{"wide runes", "日本語テスト", 4, "日…"},
		{"multibyte not sliced mid-rune", "ünïcødé-sëssion", 6, "ünïcø…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateCells(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("truncateCells(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if w := lipgloss.Width(got); tc.max > 0 && w > tc.max {
				t.Errorf("result %q is %d cells wide, over the %d budget", got, w, tc.max)
			}
		})
	}
}

// longName is wider than any panel used in these tests.
const longName = "erewhon-agent-monitor-really-long-session-name-that-overflows"

// widestLine returns the widest rendered line and its width.
func widestLine(s string) (string, int) {
	worst, width := "", 0
	for _, line := range strings.Split(s, "\n") {
		if w := lipgloss.Width(line); w > width {
			worst, width = line, w
		}
	}
	return worst, width
}

func TestRenderAgentLineFitsPanelWidth(t *testing.T) {
	base := Model{width: 44, height: 20, favorites: map[string]bool{}, lastActiveAt: map[string]time.Time{}}

	agent := mkAgent(longName, StatusRunning)
	favorited := base
	favorited.favorites = map[string]bool{longName: true}

	attached := base
	attached.attached = agent.Target()

	recentIdle := base
	recentIdle.lastActiveAt = map[string]time.Time{longName: time.Now().Add(-5 * time.Minute)}
	idleAgent := mkAgent(longName, StatusIdle)

	withActivity := base
	withActivity.showActivity = true
	activityAgent := agent
	activityAgent.LastLine = strings.Repeat("thinking about the problem ", 8)

	phantomNoSession := agent
	phantomNoSession.Presence = PresenceNoSession
	phantomNoAgent := agent
	phantomNoAgent.Presence = PresenceNoAgent

	cases := []struct {
		name   string
		model  Model
		agent  Agent
		idx    int
		indent string
	}{
		{"plain", base, agent, 1, ""},
		{"selected", base, agent, 0, ""},
		{"sub-grouped", base, agent, 1, "  "},
		{"favorited", favorited, agent, 1, ""},
		{"attached", attached, agent, 1, ""},
		{"recently idle", recentIdle, idleAgent, 1, ""},
		{"activity line", withActivity, activityAgent, 1, ""},
		{"phantom no session", base, phantomNoSession, 1, ""},
		{"phantom no agent", base, phantomNoAgent, 1, ""},
		{"phantom no agent selected", base, phantomNoAgent, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, width := widestLine(tc.model.renderAgentLine(tc.agent, tc.idx, "", tc.indent))
			if avail := tc.model.panelContentWidth(); width > avail {
				t.Errorf("line is %d cells wide, panel holds %d: %q", width, avail, line)
			}
		})
	}
}

func TestRenderAgentLineKeepsNameWhenWidthUnknown(t *testing.T) {
	// Before the first WindowSizeMsg there is no width to fit to; names must
	// survive intact rather than collapse to an ellipsis.
	m := Model{favorites: map[string]bool{}, lastActiveAt: map[string]time.Time{}}
	out := m.renderAgentLine(mkAgent(longName, StatusRunning), 1, "", "")
	if !strings.Contains(out, longName) {
		t.Errorf("name was trimmed with no known width: %q", out)
	}
}

func TestViewNeverExceedsTerminalWidth(t *testing.T) {
	m := Model{
		width:        50,
		height:       24,
		favorites:    map[string]bool{},
		lastActiveAt: map[string]time.Time{},
		collapsed:    map[string]bool{},
		agents: []Agent{
			mkAgent(longName, StatusRunning),
			mkAgent(longName+"-two", StatusWaiting),
			mkAgent("short", StatusIdle),
		},
	}
	m.buildGroups()
	// A group name far wider than the panel, plus a long-running backend failure.
	m.groups[0].Name = "a-very-long-group-name-that-does-not-fit-in-the-panel-at-all"
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // keep the failure log out of the real state dir
	recordBackendErr("nous", "fetch", errLong)
	defer clearBackendErr("nous", "fetch")

	line, width := widestLine(m.View())
	if width > m.width {
		t.Errorf("View line is %d cells wide, terminal is %d: %q", width, m.width, line)
	}
}

func TestViewShowsBackendIssueInsteadOfPrinting(t *testing.T) {
	m := Model{width: 90, height: 24, favorites: map[string]bool{}, lastActiveAt: map[string]time.Time{}, collapsed: map[string]bool{}}
	m.agents = []Agent{mkAgent("one", StatusIdle)}
	m.buildGroups()

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	recordBackendErr("nous", "fetch", errLong)
	defer clearBackendErr("nous", "fetch")

	out := stripANSI(m.View())
	if !strings.Contains(out, "nous fetch") {
		t.Errorf("backend issue missing from the footer:\n%s", out)
	}
	if !strings.Contains(out, "HTTP 401") {
		t.Errorf("footer dropped the underlying cause:\n%s", out)
	}
}

// errLong is a realistic backend failure: long enough to need trimming.
var errLong = errors.New("/api/notebooks: HTTP 401: Missing API key. Include Authorization: Bearer <key>")
