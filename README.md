# Agent Monitor

A terminal UI for tracking multiple Claude Code agents running in tmux sessions.

## Features

- **Real-time status tracking**: Detects if agents are running, waiting for input, or idle
- **Live terminal view**: Right pane shows actual tmux session (not a snapshot)
- **Nested tmux support**: Outer tmux uses separate socket and prefix (`C-a`) to avoid conflicts
- **Quick navigation**: Switch between agents with j/k, focus right pane with l

## Installation

```bash
# Build
cd /path/to/agent-monitor
go build -o agent-monitor .

# Install
cp agent-monitor ~/.local/bin/
cp agent-monitor-session ~/.local/bin/
cp focus-agent-monitor ~/.local/bin/
cp tmux-outer.conf ~/.config/agent-monitor-tmux.conf
```

## Usage

### Launch the monitor session

```bash
agent-monitor-session
```

This creates an outer tmux session with:
- Left pane: agent-monitor TUI (narrow)
- Right pane: live view of selected agent (wide)

```
┌──────────────────────────────────────────────────────────┐
│ Outer tmux (C-a prefix, separate socket)                 │
├─────────────────┬────────────────────────────────────────┤
│ Agent Monitor   │                                        │
│ 7 agents        │  [live tmux session attached here]     │
│                 │                                        │
│ > ● repo-a ◀   │  $ claude                              │
│   ◐ repo-b     │  > working on feature...               │
│   ○ repo-c     │                                        │
│   ○ repo-d     │                                        │
│                 │                                        │
│ j/k:nav ⏎:att  │                                        │
└─────────────────┴────────────────────────────────────────┘
```

### Key bindings in agent-monitor (left pane)

| Key | Action |
|-----|--------|
| `j` / `↓` | Move cursor down |
| `k` / `↑` | Move cursor up |
| `Enter` | Attach selected agent to right pane |
| `l` / `→` | Focus the right pane |
| `r` | Refresh agent list |
| `q` | Quit |

### Key bindings in outer tmux

| Key | Action |
|-----|--------|
| `C-a h` | Focus left pane (agent-monitor) |
| `C-a l` | Focus right pane (agent view) |
| `C-a q` | Quit agent-monitor session |

### Returning from inner tmux to agent-monitor

Add this to your regular tmux config (`~/.tmux.conf`):

```tmux
# Press C-b M-m (or F12) to jump back to agent-monitor
bind-key M-m run-shell "focus-agent-monitor"
bind-key F12 run-shell "focus-agent-monitor"
```

Then from any inner tmux session, press `C-b M-m` or `C-b F12` to return focus to the agent-monitor pane.

## Status Indicators

| Symbol | Status | Description |
|--------|--------|-------------|
| `●` (green) | Running | Agent is actively working |
| `◐` (yellow) | Waiting | Agent needs user input (permission prompt) |
| `○` (gray) | Idle | Agent is ready for a new command |
| `✕` (red) | Error | Agent encountered an error |

## How It Works

1. **Outer tmux session**: Runs on a separate socket (`-L agent-monitor`) with its own config and prefix (`C-a`) to avoid conflicts with your regular tmux
2. **Agent detection**: Scans all panes in the default tmux socket for processes named `claude`
3. **Status detection**: Captures pane content to detect state based on Claude Code output patterns
4. **Live attachment**: When you press Enter, the right pane attaches to the selected agent's actual tmux session

## Standalone modes

```bash
# Just list agents (for scripts/status bars)
agent-monitor --list

# Run TUI without outer tmux integration
agent-monitor --no-attach
```

## Files

| File | Location | Purpose |
|------|----------|---------|
| `agent-monitor` | `~/.local/bin/` | Main TUI binary |
| `agent-monitor-session` | `~/.local/bin/` | Launcher script |
| `focus-agent-monitor` | `~/.local/bin/` | Helper to return focus |
| `agent-monitor-tmux.conf` | `~/.config/` | Outer tmux config |

## Requirements

- Go 1.21+
- tmux
- Claude Code running in tmux sessions
