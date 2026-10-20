package cli

import (
	"strings"
	"testing"
	"time"
)

var wardenNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func intp(i int) *int { return &i }

func wardenDoc(t *testing.T, body string) *wardenStatus {
	t.Helper()
	d, err := parseWardenStatus([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

const heartbeatDoc = `{"vm":"work-cfa","queue_depth":null,"current":{"id":null,"title":null,"state":"running","started_at":"2026-10-19T11:45:30Z"},"heartbeat_at":"2026-10-19T11:59:20Z","commits_since_start":6,"tests":null,"reason_code":"OK","last_event_at":"2026-10-19T11:45:30Z","paused":false}`

func sample(doc *wardenStatus, err error) wardenSample {
	return wardenSample{cfg: wardenCfg{Label: "work-cfa", Host: "work-cfa.m.bcc.sh", Attach: "ssh -t work-cfa"}, doc: doc, err: err, at: wardenNow}
}

func TestParseWardenStatusNullsAndSupervisorShape(t *testing.T) {
	d := wardenDoc(t, heartbeatDoc)
	if d.VM != "work-cfa" || d.QueueDepth != nil || d.Tests != nil || *d.CommitsSinceStart != 6 {
		t.Fatalf("unexpected doc %+v", d)
	}
	full := `{"vm":"lab-7","queue_depth":3,"current":{"id":"t-017","title":"Add the export","state":"blocked","started_at":"2026-10-19T11:30:00Z"},"heartbeat_at":"2026-10-19T11:59:30Z","commits_since_start":2,"tests":{"pass":40,"fail":1},"reason_code":"TESTS_FAILING","last_event_at":"2026-10-19T11:30:00Z","paused":false}`
	d = wardenDoc(t, full)
	if *d.QueueDepth != 3 || d.Tests["fail"] != 1 || *d.Current.Title != "Add the export" {
		t.Fatalf("unexpected doc %+v", d)
	}
}

func TestParseWardenStatusRejectsJunk(t *testing.T) {
	for _, body := range []string{"", "not json", `{"vm":"x"}`, `{"vm":"x","heartbeat_at":"2026-10-19T11:59:30Z","reason_code":"OK","extra":"free text"}`} {
		if _, err := parseWardenStatus([]byte(body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

func TestWardenRowMapping(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(d *wardenStatus)
		status AgentStatus
		wait   WaitReason
		line   string
	}{
		{"running", func(d *wardenStatus) {}, StatusRunning, WaitNone, "commits 6 · hb 40s · OK"},
		{"idle", func(d *wardenStatus) { d.Current = nil; d.ReasonCode = "NO_TASKS" }, StatusIdle, WaitNone, "commits 6 · hb 40s · NO_TASKS"},
		{"done", func(d *wardenStatus) { d.Current.State = "done" }, StatusIdle, WaitNone, ""},
		{"blocked needs human", func(d *wardenStatus) { d.Current.State = "blocked"; d.ReasonCode = "NEEDS_HUMAN" }, StatusWaiting, WaitApproval, "NEEDS_HUMAN"},
		{"blocked tests failing", func(d *wardenStatus) {
			d.Current.State = "blocked"
			d.ReasonCode = "TESTS_FAILING"
			d.Tests = map[string]int{"pass": 40, "fail": 2}
		}, StatusWaiting, WaitInput, "t 40/2"},
		{"paused wins", func(d *wardenStatus) { d.Paused = true; d.Current.State = "blocked"; d.ReasonCode = "NEEDS_HUMAN" }, StatusIdle, WaitNone, "paused · "},
		{"stale heartbeat", func(d *wardenStatus) { d.HeartbeatAt = wardenNow.Add(-11 * time.Minute) }, StatusError, WaitNone, "stale: commits 6 · hb 11m"},
		{"label mismatch", func(d *wardenStatus) { d.VM = "other" }, StatusError, WaitNone, "vm label mismatch"},
		{"queue shown when present", func(d *wardenStatus) { d.QueueDepth = intp(0) }, StatusRunning, WaitNone, "· q 0"},
		{"null commits", func(d *wardenStatus) { d.CommitsSinceStart = nil }, StatusRunning, WaitNone, "commits ?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := wardenDoc(t, heartbeatDoc)
			tc.mut(d)
			a := wardenRow(sample(d, nil), wardenNow)
			if a.Status != tc.status || a.WaitReasonOrNone() != tc.wait {
				t.Fatalf("got %s/%s want %s/%s (line %q)", a.Status, a.WaitReasonOrNone(), tc.status, tc.wait, a.LastLine)
			}
			if !strings.Contains(a.LastLine, tc.line) {
				t.Fatalf("line %q lacks %q", a.LastLine, tc.line)
			}
			if a.Type != AgentWarden || a.Presence != PresenceActive || a.Session != "work-cfa" || a.Host != "work-cfa.m.bcc.sh" || a.Attach != "ssh -t work-cfa" {
				t.Fatalf("row identity wrong: %+v", a)
			}
		})
	}
}

func TestWardenRowErrorsAndPolling(t *testing.T) {
	a := wardenRow(sample(nil, errSSH("ssh exit 255")), wardenNow)
	if a.Status != StatusError || a.LastLine != "unreachable: ssh exit 255" {
		t.Fatalf("got %+v", a)
	}
	s := sample(nil, nil)
	s.at = time.Time{}
	a = wardenRow(s, wardenNow)
	if a.Status != StatusUnknown || a.LastLine != "polling" {
		t.Fatalf("got %+v", a)
	}
}

type errSSH string

func (e errSSH) Error() string { return string(e) }

func TestWardenStaleWindowIsConfigurable(t *testing.T) {
	d := wardenDoc(t, heartbeatDoc)
	d.HeartbeatAt = wardenNow.Add(-3 * time.Minute)
	s := sample(d, nil)
	s.cfg.StaleMin = 2
	if a := wardenRow(s, wardenNow); a.Status != StatusError {
		t.Fatalf("2-minute window: got %s", a.Status)
	}
	s.cfg.StaleMin = 0
	if a := wardenRow(s, wardenNow); a.Status != StatusRunning {
		t.Fatalf("default window: got %s", a.Status)
	}
}

func TestAttachCommand(t *testing.T) {
	local := Agent{Session: "meta", Window: 0, Pane: 1, Presence: PresenceActive}
	if got := attachCommand(local); got != "unset TMUX; exec tmux attach-session -t 'meta:0.1'" {
		t.Fatalf("local: %q", got)
	}
	phantom := Agent{Session: "meta", Presence: PresenceNoAgent}
	if got := attachCommand(phantom); got != "unset TMUX; exec tmux attach-session -t 'meta'" {
		t.Fatalf("phantom: %q", got)
	}
	own := Agent{Session: "work-cfa", Type: AgentWarden, Attach: "ssh -t work-cfa tmux new -A -s main"}
	if got := attachCommand(own); got != own.Attach {
		t.Fatalf("own attach: %q", got)
	}
	panel := Agent{Session: "work-da", Type: AgentWarden}
	if got := attachCommand(panel); !strings.HasSuffix(got, " 'warden-status' 'work-da'") {
		t.Fatalf("panel: %q", got)
	}
	remote := Agent{Session: "x", Host: "elsewhere", Attach: "ssh elsewhere"}
	if got := attachCommand(remote); got != "ssh elsewhere" {
		t.Fatalf("remote webhook row: %q", got)
	}
}

func TestParseWardensConfig(t *testing.T) {
	cfgs, err := parseWardensConfig([]byte(`
wardens:
  - label: work-cfa
    host: work-cfa.m.bcc.sh
    key: ~/.ssh/warden-work-cfa
    attach: ssh -t work-cfa.m.bcc.sh tmux new -A -s main
  - label: work-da
    host: 10.0.0.9
    user: gate
    port: 2222
    poll_s: 60
    stale_min: 5
    notify_ntfy: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("got %d wardens", len(cfgs))
	}
	a, b := cfgs[0], cfgs[1]
	if a.user() != "warden" || a.poll() != 30*time.Second || a.stale() != 10*time.Minute || strings.HasPrefix(a.Key, "~") {
		t.Fatalf("defaults wrong: %+v", a)
	}
	if b.user() != "gate" || b.Port != 2222 || b.poll() != time.Minute || b.stale() != 5*time.Minute || !boolOr(b.NotifyNtfy, false) {
		t.Fatalf("overrides wrong: %+v", b)
	}
	args := wardenSSHArgs(b, "status")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-F /dev/null", "BatchMode=yes", "IdentitiesOnly=yes", "ClearAllForwardings=yes", "-p 2222", "gate@10.0.0.9 status"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args lack %q: %s", want, joined)
		}
	}
	if args[len(args)-1] != "status" {
		t.Errorf("verb must be the whole command: %v", args)
	}
	for _, bad := range []string{"wardens:\n  - host: h\n", "wardens:\n  - label: a\n    host: h\n  - label: a\n    host: h2\n", "wardens: [\n"} {
		if _, err := parseWardensConfig([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestWardenStoreRowsAndNtfyDefault(t *testing.T) {
	ws := newWardenStore([]wardenCfg{{Label: "b", Host: "hb"}, {Label: "a", Host: "ha", NotifyNtfy: func() *bool { v := true; return &v }()}})
	rows := ws.Agents(wardenNow)
	if len(rows) != 2 || rows[0].Session != "b" || rows[1].Session != "a" {
		t.Fatalf("config order not kept: %+v", rows)
	}
	if rows[0].Status != StatusUnknown {
		t.Fatalf("unpolled row should be unknown, got %s", rows[0].Status)
	}
	if ws.NtfyEnabled("b") || !ws.NtfyEnabled("a") || ws.NtfyEnabled("zzz") {
		t.Fatal("ntfy default should be off, opt-in per warden")
	}
	ws.set("b", wardenDoc(t, strings.Replace(heartbeatDoc, `"vm":"work-cfa"`, `"vm":"b"`, 1)), nil, wardenNow)
	if got := ws.Agents(wardenNow)[0]; got.Status != StatusRunning {
		t.Fatalf("after set: %s %q", got.Status, got.LastLine)
	}
	var nilStore *WardenStore
	if nilStore.Agents(wardenNow) != nil || nilStore.NtfyEnabled("a") {
		t.Fatal("nil store must be inert")
	}
}

func TestWardensGroupInBuildGroups(t *testing.T) {
	m := &Model{
		config: Config{Groups: []GroupConfig{{Name: "Work", Sessions: []string{"work-cfa"}}}},
		agents: []Agent{
			mkAgent("meta", StatusRunning),
			{Name: "work-cfa", Session: "work-cfa", Type: AgentWarden, Status: StatusRunning, Presence: PresenceActive},
			{Name: "work-da", Session: "work-da", Type: AgentWarden, Status: StatusError, Presence: PresenceActive},
		},
	}
	m.buildGroups()
	names := groupNames(m)
	want := []string{"Work", "Wardens", "Other"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("groups %v want %v", names, want)
	}
	if m.groups[1].Items[0].Agent.Session != "work-da" {
		t.Fatalf("unclaimed warden should be in Wardens: %+v", m.groups[1].Items)
	}
}

func TestWardenPanelShowsOnlyDocumentFields(t *testing.T) {
	d := wardenDoc(t, heartbeatDoc)
	co := &coordinatorState{LastNudgeAt: func() *time.Time { x := wardenNow.Add(-7 * time.Minute); return &x }(), Failures: 2}
	out := renderWardenPanel(sample(d, nil), co, wardenNow)
	for _, want := range []string{"warden work-cfa", "running", "heartbeat  40s ago", "commits    6", "queue      ?", "last nudge 7m ago", "failures   2"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	d.Current.State = "blocked"
	d.ReasonCode = "NEEDS_HUMAN"
	out = renderWardenPanel(sample(d, nil), nil, wardenNow)
	for _, want := range []string{"◕ waiting:approval", "reason     NEEDS_HUMAN"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
}
