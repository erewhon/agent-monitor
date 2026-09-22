package cli

import (
	"strings"
	"testing"
)

func TestSessionPlanReinvokesSelfCommand(t *testing.T) {
	old := SelfCommand
	defer func() { SelfCommand = old }()
	SelfCommand = []string{"/opt/bin/pitf", "monitor"}
	plan := sessionPlan("agent-monitor", "/c/outer.conf", 210, 55, []string{"--groups", "Tech,Self hosting"})
	if len(plan) != 5 {
		t.Fatalf("plan has %d steps", len(plan))
	}
	join := func(i int) string { return strings.Join(plan[i], " ") }
	if !strings.Contains(join(0), "new-session -d -s agent-monitor -x 210 -y 55") || !strings.Contains(join(0), "-f /c/outer.conf") {
		t.Fatalf("step 0: %s", join(0))
	}
	if !strings.Contains(join(2), "unset TMUX; exec '/opt/bin/pitf' 'monitor' '--socket=agent-monitor' '--groups' 'Tech,Self hosting'") {
		t.Fatalf("inner TUI must be launched through the self command with flags quoted: %s", join(2))
	}
	if !strings.Contains(join(3), "'/opt/bin/pitf' 'monitor' 'placeholder'") {
		t.Fatalf("placeholder must go through the self command: %s", join(3))
	}
	if !strings.HasSuffix(join(4), "select-pane -t agent-monitor:0.0") {
		t.Fatalf("step 4: %s", join(4))
	}
}

func TestOuterConfTemplateFillsStatsCommand(t *testing.T) {
	old := SelfCommand
	defer func() { SelfCommand = old }()
	SelfCommand = []string{"/x/agent-monitor"}
	conf := strings.ReplaceAll(outerConfTemplate, "@STATS_CMD@", selfShell("stats"))
	if strings.Contains(conf, "@STATS_CMD@") || !strings.Contains(conf, `#('/x/agent-monitor' 'stats')`) {
		t.Fatalf("stats command not filled: %s", conf)
	}
	if !strings.Contains(conf, `set -g prefix 'C-\'`) {
		t.Fatal("embedded template lost the outer prefix")
	}
}

func TestStatsHelpers(t *testing.T) {
	if g := barGauge(50, 6); g != "███░░░" {
		t.Fatalf("barGauge(50) = %q", g)
	}
	if g := barGauge(100, 6); g != "██████" {
		t.Fatalf("barGauge(100) = %q", g)
	}
	// scale floor is 4 → load 2.0 is index 3, load 4.0 is index 7
	if s := sparkline([]float64{0, 2, 4}, 8); s != "▁▄█" {
		t.Fatalf("sparkline = %q", s)
	}
	if !strings.Contains(filterEnv([]string{"A=1", "TMUX=/x", "B=2"}, "TMUX")[1], "B=") {
		t.Fatal("filterEnv did not drop TMUX")
	}
}
