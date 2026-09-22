package cli

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// SelfCommand is how this program re-invokes itself inside the outer tmux
// (the TUI pane, the placeholder pane, the status-bar stats). Standalone it
// is the executable path; a host that mounts agent-monitor as a subcommand
// (pitf) sets it to e.g. {"/path/pitf", "monitor"} so the re-invocations
// reach the mount rather than the host's root command.
var SelfCommand []string

func selfCommand() []string {
	if len(SelfCommand) > 0 {
		return SelfCommand
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "agent-monitor"
	}
	return []string{exe}
}

// selfShell renders the self command plus args as a shell fragment.
func selfShell(args ...string) string {
	parts := append(append([]string{}, selfCommand()...), args...)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	return strings.Join(quoted, " ")
}

const outerSession = "agent-monitor"

//go:embed tmux-outer.conf
var outerConfTemplate string

// outerConfPath is where the outer tmux config lives; written from the
// embedded template on first use so `agent-monitor` works from a fresh
// install with no companion files.
func outerConfPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	p := filepath.Join(base, "agent-monitor-tmux.conf")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	conf := strings.ReplaceAll(outerConfTemplate, "@STATS_CMD@", selfShell("stats"))
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// insideOuter reports whether this process is already running in a pane of
// the outer tmux server on socket. The launcher unsets TMUX for the inner
// process but tmux still sets TMUX_PANE, and only the server that owns that
// pane can describe it.
func insideOuter(socket string) bool {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" || socket == "" {
		return false
	}
	return exec.Command("tmux", "-L", socket, "display-message", "-p", "-t", pane, "#{pane_id}").Run() == nil
}

// outerRunning reports whether the outer session already exists.
func outerRunning(socket string) bool {
	return exec.Command("tmux", "-L", socket, "has-session", "-t", outerSession).Run() == nil
}

// sessionPlan is the tmux command sequence that builds the outer layout;
// separated from execution so it can be tested.
func sessionPlan(socket, conf string, cols, lines int, extraArgs []string) [][]string {
	inner := "unset TMUX; exec " + selfShell(append([]string{"--socket=" + socket}, extraArgs...)...)
	return [][]string{
		{"tmux", "-L", socket, "-f", conf, "new-session", "-d", "-s", outerSession, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(lines)},
		{"tmux", "-L", socket, "split-window", "-h", "-t", outerSession, "-l", "80%"},
		{"tmux", "-L", socket, "send-keys", "-t", outerSession + ":0.0", inner, "Enter"},
		{"tmux", "-L", socket, "send-keys", "-t", outerSession + ":0.1", selfShell("placeholder"), "Enter"},
		{"tmux", "-L", socket, "select-pane", "-t", outerSession + ":0.0"},
	}
}

// launchOuterSession creates the outer tmux session (left: this TUI, right: the
// attached agent) or attaches to the existing one, then replaces the
// process with `tmux attach`. extraArgs are forwarded to the inner TUI.
func launchOuterSession(socket string, extraArgs []string) error {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	if !outerRunning(socket) {
		conf, err := outerConfPath()
		if err != nil {
			return err
		}
		cols, lines, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || cols <= 0 || lines <= 0 {
			cols, lines = 200, 50
		}
		for _, argv := range sessionPlan(socket, conf, cols, lines, extraArgs) {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("%s: %w", strings.Join(argv[1:], " "), err)
			}
		}
	} else if len(extraArgs) > 0 {
		fmt.Fprintf(os.Stderr, "agent-monitor session already running; attaching (flags %v ignored)\n", extraArgs)
	}
	attach := []string{"tmux", "-L", socket, "attach-session", "-t", outerSession}
	env := os.Environ()
	// A nested attach must not think it is already inside a tmux client.
	env = filterEnv(env, "TMUX")
	return syscall.Exec(tmuxPath, attach, env)
}

func filterEnv(env []string, drop string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, drop+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// runPlaceholder draws the right pane's idle message and waits; the TUI's
// attach respawns the pane over it.
func runPlaceholder() error {
	fmt.Print("\033[2J")
	cols, lines, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols <= 0 {
		cols, lines = 80, 24
	}
	row, col := lines/2, cols/2
	fmt.Printf("\033[%d;%dH\033[35m◇\033[0m\033[2m  Select an agent from the left\033[0m", row-1, max(col-15, 1))
	fmt.Printf("\033[%d;%dH\033[2mEnter to attach, l to focus\033[0m", row+1, max(col-10, 1))
	for {
		time.Sleep(24 * time.Hour)
	}
}
