# Agent Monitor

A terminal UI for tracking multiple Claude Code agents running in tmux sessions.

## Features

- **Real-time status tracking**: Detects if agents are running, waiting for input, or idle
- **Split-pane preview**: Shows the current output of the selected agent
- **Quick navigation**: Switch between agents with j/k keys
- **Direct attach**: Press Enter to switch to the agent's tmux pane

## Installation

```bash
# Build
go build -o agent-monitor .

# Install to your PATH
cp agent-monitor ~/.local/bin/
```

## Usage

```bash
# Interactive TUI (default)
agent-monitor

# List agents and exit (for scripts/status bars)
agent-monitor --list

# Without preview pane
agent-monitor --preview=false
```

## Key Bindings

| Key | Action |
|-----|--------|
| `j` / `↓` | Move cursor down |
| `k` / `↑` | Move cursor up |
| `Enter` | Attach to selected agent's tmux pane |
| `p` | Toggle preview pane |
| `r` | Refresh agent list |
| `q` | Quit |

## Status Indicators

| Symbol | Status | Description |
|--------|--------|-------------|
| `●` (green) | Running | Agent is actively working |
| `◐` (yellow) | Waiting | Agent needs user input (permission prompt) |
| `○` (gray) | Idle | Agent is ready for a new command |
| `✕` (red) | Error | Agent encountered an error |

## How It Works

1. Scans all tmux panes for processes named `claude`
2. Captures pane content to detect status based on output patterns
3. Updates every 2 seconds automatically

## Recommended Setup

Run agent-monitor in a dedicated tmux pane or terminal:

```
┌─────────────────────────────────────────────────────┐
│ Outer tmux session                                  │
├────────────────┬────────────────────────────────────┤
│ agent-monitor  │                                    │
│ (left pane)    │  Active agent tmux session         │
│                │  (right pane, attached via Enter)  │
│ ● repo-a ◀    │                                    │
│ ◐ repo-b      │  [agent output here]               │
│ ○ repo-c      │                                    │
└────────────────┴────────────────────────────────────┘
```

## Requirements

- Go 1.21+
- tmux
- Claude Code running in tmux sessions

## License

MIT
