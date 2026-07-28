package main

import (
	"strings"
	"testing"
	"time"
)

// pane joins fixture lines into the shape tmux capture-pane returns.
func pane(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestClassifyClaudeStatus(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantStatus AgentStatus
		wantWait   WaitReason
	}{
		{
			name: "permission dialog is an approval gate",
			content: pane(
				"● Bash(rm -rf /tmp/scratch)",
				"╭──────────────────────────────────────╮",
				"│ Bash command                         │",
				"│ rm -rf /tmp/scratch                  │",
				"│ Do you want to proceed?              │",
				"│ ❯ 1. Yes                             │",
				"│   2. Yes, and don't ask again        │",
				"│   3. No, and tell Claude what to do  │",
				"╰──────────────────────────────────────╯",
			),
			wantStatus: StatusWaiting,
			wantWait:   WaitApproval,
		},
		{
			name: "edit confirmation is an approval gate",
			content: pane(
				"● Update(main.go)",
				"│ Do you want to make this edit to main.go? │",
				"│ ❯ 1. Yes                                  │",
				"│   2. No, and tell Claude what to do       │",
			),
			wantStatus: StatusWaiting,
			wantWait:   WaitApproval,
		},
		{
			name: "multiple-choice question is an input prompt",
			content: pane(
				"● Which database should the service use?",
				"╭──────────────────────────────────────╮",
				"│ ❯ 1. Postgres                        │",
				"│   2. SQLite                          │",
				"│   3. MySQL                           │",
				"╰──────────────────────────────────────╯",
			),
			wantStatus: StatusWaiting,
			wantWait:   WaitInput,
		},
		{
			name: "free-text prompt chrome is an input prompt",
			content: pane(
				"● What should the retry budget be?",
				"> ",
				"Press Enter to send",
			),
			wantStatus: StatusWaiting,
			wantWait:   WaitInput,
		},
		{
			name: "active spinner outranks the prompt line below it",
			content: pane(
				"✻ Frobnicating… (12s · ↑ 1.2k tokens · esc to interrupt)",
				"⏵⏵ accept edits on",
			),
			wantStatus: StatusRunning,
			wantWait:   WaitNone,
		},
		{
			name: "idle at the prompt",
			content: pane(
				"● Done — the build passes.",
				"⏵⏵ accept edits on (shift+tab to cycle)",
			),
			wantStatus: StatusIdle,
			wantWait:   WaitNone,
		},
		{
			name: "prose mentioning approval does not trigger a wait",
			content: pane(
				"● I would allow that, but you should approve the change first.",
				"⏵⏵ accept edits on",
			),
			wantStatus: StatusIdle,
			wantWait:   WaitNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyClaudeStatus(tc.content)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v", got.Status, tc.wantStatus)
			}
			if got.Wait != tc.wantWait {
				t.Errorf("wait = %q, want %q", got.Wait, tc.wantWait)
			}
		})
	}
}

func TestClassifyCodexStatus(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantStatus AgentStatus
		wantWait   WaitReason
	}{
		{
			// Regression guard: "• Ran" and words like "allow" persist in
			// Codex scrollback after it returns to idle. Anchoring approval
			// markers at line start keeps stale output from reading as a wait.
			name: "stale tool output with an idle prompt stays idle",
			content: pane(
				"• Ran allow-check.sh",
				"  Allowed hosts verified",
				"›",
				"? for shortcuts   92% context left",
			),
			wantStatus: StatusIdle,
			wantWait:   WaitNone,
		},
		{
			name: "approval prompt",
			content: pane(
				"• Run `rm -rf build`?",
				"│ ❯ 1. Allow                          │",
				"│   2. Deny                           │",
			),
			wantStatus: StatusWaiting,
			wantWait:   WaitApproval,
		},
		{
			name: "actively working",
			content: pane(
				"• Refactoring the parser (19s • esc to interrupt)",
			),
			wantStatus: StatusRunning,
			wantWait:   WaitNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCodexStatus(tc.content)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v", got.Status, tc.wantStatus)
			}
			if got.Wait != tc.wantWait {
				t.Errorf("wait = %q, want %q", got.Wait, tc.wantWait)
			}
		})
	}
}

func TestClassifyCrushStatus(t *testing.T) {
	approval := classifyCrushStatus(pane(
		"Crush wants to run: git push --force",
		"│ ❯ Allow   Deny │",
	))
	if approval.Status != StatusWaiting || approval.Wait != WaitApproval {
		t.Errorf("approval dialog = (%v, %q), want (waiting, approval)", approval.Status, approval.Wait)
	}

	// Regression guard: the words used to match anywhere in the pane.
	prose := classifyCrushStatus(pane(
		"The firewall config will allow or deny each host.",
		"crush>",
	))
	if prose.Status != StatusIdle {
		t.Errorf("prose mentioning allow/deny = %v, want idle", prose.Status)
	}
}

func TestClassifyOpenCodeStatus(t *testing.T) {
	approval := classifyOpenCodeStatus(pane(
		"Run shell command?",
		"[ allow ]  [ deny ]",
	))
	if approval.Status != StatusWaiting || approval.Wait != WaitApproval {
		t.Errorf("approval dialog = (%v, %q), want (waiting, approval)", approval.Status, approval.Wait)
	}

	running := classifyOpenCodeStatus(pane(
		"✱ Glob **/*.go",
		"esc interrupt",
	))
	if running.Status != StatusRunning || running.Wait != WaitNone {
		t.Errorf("running pane = (%v, %q), want (running, none)", running.Status, running.Wait)
	}
}

func TestParseWebhookStatusFull(t *testing.T) {
	tests := []struct {
		in         string
		wantStatus AgentStatus
		wantWait   WaitReason
	}{
		{"waiting", StatusWaiting, WaitNone},
		{"waiting-approval", StatusWaiting, WaitApproval},
		{"waiting_input", StatusWaiting, WaitInput},
		{"WAITING-APPROVAL", StatusWaiting, WaitApproval},
		{"running", StatusRunning, WaitNone},
		{"idle", StatusIdle, WaitNone},
		{"nonsense", StatusUnknown, WaitNone},
	}
	for _, tc := range tests {
		gotStatus, gotWait := parseWebhookStatusFull(tc.in)
		if gotStatus != tc.wantStatus || gotWait != tc.wantWait {
			t.Errorf("parseWebhookStatusFull(%q) = (%v, %q), want (%v, %q)",
				tc.in, gotStatus, gotWait, tc.wantStatus, tc.wantWait)
		}
	}
	// The legacy single-return wrapper must keep reporting plain statuses.
	if parseWebhookStatus("waiting-approval") != StatusWaiting {
		t.Error("parseWebhookStatus lost the base status for a compound value")
	}
}

func TestWebhookResolvedWait(t *testing.T) {
	tests := []struct {
		name  string
		state WebhookState
		want  WaitReason
	}{
		{
			name:  "explicit wait_reason wins",
			state: WebhookState{Status: "waiting", WaitReason: "approval"},
			want:  WaitApproval,
		},
		{
			name:  "compound status",
			state: WebhookState{Status: "waiting-input"},
			want:  WaitInput,
		},
		{
			name:  "PreToolUse hook implies an approval gate",
			state: WebhookState{Status: "waiting", HookEvent: "PreToolUse"},
			want:  WaitApproval,
		},
		{
			name:  "permission notification is an approval gate",
			state: WebhookState{Status: "waiting", HookEvent: "Notification", Detail: "Claude needs your permission to use Bash"},
			want:  WaitApproval,
		},
		{
			name:  "plain notification is an input prompt",
			state: WebhookState{Status: "waiting", HookEvent: "Notification", Detail: "Claude is waiting for your input"},
			want:  WaitInput,
		},
		{
			name:  "non-waiting status has no reason",
			state: WebhookState{Status: "running", HookEvent: "PreToolUse"},
			want:  WaitNone,
		},
		{
			name:  "bare waiting leaves the reason to the pane heuristic",
			state: WebhookState{Status: "waiting"},
			want:  WaitNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.resolvedWait(); got != tc.want {
				t.Errorf("resolvedWait() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMergeWebhookStatePrefersHookReason(t *testing.T) {
	ws := newWebhookStore()
	ws.Set(WebhookState{
		Session:   "alpha",
		Status:    "waiting",
		HookEvent: "PreToolUse",
		Timestamp: time.Now(),
	})
	// The pane heuristic guessed "input"; the hook knows it's an approval gate.
	agents := []Agent{{Session: "alpha", Status: StatusWaiting, Wait: WaitInput}}
	mergeWebhookState(agents, ws)
	if agents[0].Wait != WaitApproval {
		t.Errorf("wait = %q, want approval (hook state must win)", agents[0].Wait)
	}

	// A bare "waiting" carries no reason, so the pane's guess must survive.
	ws.Set(WebhookState{Session: "beta", Status: "waiting", Timestamp: time.Now()})
	agents = []Agent{{Session: "beta", Status: StatusWaiting, Wait: WaitApproval}}
	mergeWebhookState(agents, ws)
	if agents[0].Wait != WaitApproval {
		t.Errorf("wait = %q, want approval (pane reason must survive a bare waiting)", agents[0].Wait)
	}

	// Leaving the waiting state clears any stale reason.
	ws.Set(WebhookState{Session: "gamma", Status: "running", Timestamp: time.Now()})
	agents = []Agent{{Session: "gamma", Status: StatusWaiting, Wait: WaitApproval}}
	mergeWebhookState(agents, ws)
	if agents[0].Wait != WaitNone {
		t.Errorf("wait = %q, want none once the agent is running", agents[0].Wait)
	}
}

func TestWaitReasonOrNoneMasksStaleReason(t *testing.T) {
	a := Agent{Status: StatusRunning, Wait: WaitApproval}
	if got := a.WaitReasonOrNone(); got != WaitNone {
		t.Errorf("WaitReasonOrNone() = %q on a running agent, want none", got)
	}
	if got := toAPIAgent(a).WaitReason; got != "" {
		t.Errorf("apiAgent.WaitReason = %q on a running agent, want empty", got)
	}
}

func TestAPIAgentKeepsPlainWaitingStatus(t *testing.T) {
	// Backward compatibility: wait_reason is additive — "status" must still
	// read "waiting" for clients that know nothing about sub-states.
	got := toAPIAgent(Agent{Status: StatusWaiting, Wait: WaitApproval, Presence: PresenceActive})
	if got.Status != "waiting" {
		t.Errorf("status = %q, want %q", got.Status, "waiting")
	}
	if got.WaitReason != "approval" {
		t.Errorf("wait_reason = %q, want %q", got.WaitReason, "approval")
	}
}

func TestNotifyEventForWait(t *testing.T) {
	if got := notifyEventForWait(WaitApproval); got != NotifyApproval {
		t.Errorf("approval → %q, want %q", got, NotifyApproval)
	}
	for _, w := range []WaitReason{WaitInput, WaitNone} {
		if got := notifyEventForWait(w); got != NotifyWaiting {
			t.Errorf("%q → %q, want %q", w, got, NotifyWaiting)
		}
	}
	approval := Notification{AgentName: "alpha", Badge: "cc", Event: NotifyApproval}
	if !strings.Contains(approval.Title(), "needs approval") {
		t.Errorf("approval title = %q, want it to say \"needs approval\"", approval.Title())
	}
	// AGENT_MONITOR_EVENT is what custom notify commands branch on, so the
	// two sub-states must stay distinguishable on the wire.
	if string(NotifyApproval) == string(NotifyWaiting) {
		t.Error("approval and waiting must be distinct event values")
	}
}

func TestStatusBucketsSplitWaiting(t *testing.T) {
	approval := mkAgent("appr1", StatusWaiting)
	approval.Wait = WaitApproval
	input := mkAgent("input1", StatusWaiting)
	input.Wait = WaitInput
	unknown := mkAgent("wait1", StatusWaiting) // no reason detected

	m := &Model{
		groupByStatus: true,
		lastActiveAt:  map[string]time.Time{},
		agents:        []Agent{input, unknown, approval},
	}
	m.buildGroups()

	want := []string{bucketApproval, bucketWaiting}
	got := groupNames(m)
	if len(got) != len(want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Approval sorts above plain waiting, and un-reasoned waits stay generic.
	if n := len(flattenItems(m.groups[0].Items)); n != 1 {
		t.Errorf("approval bucket has %d agents, want 1", n)
	}
	if n := len(flattenItems(m.groups[1].Items)); n != 2 {
		t.Errorf("waiting bucket has %d agents, want 2", n)
	}
}

func TestStateOfKeysTransitionsOnWaitReason(t *testing.T) {
	input := Agent{Status: StatusWaiting, Wait: WaitInput}
	approval := Agent{Status: StatusWaiting, Wait: WaitApproval}
	if stateOf(input) == stateOf(approval) {
		t.Error("input and approval waits must be distinct transition states")
	}
	// A stale reason on a non-waiting agent must not split the state.
	if stateOf(Agent{Status: StatusIdle, Wait: WaitApproval}) != stateOf(Agent{Status: StatusIdle}) {
		t.Error("idle agents must compare equal regardless of a stale wait reason")
	}
}
