# Agent Monitor

A terminal UI for tracking multiple coding agents running in tmux sessions. Supports **Claude Code**, **OpenCode**, and **Crush**.

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
| `s` | Toggle group-by-status (bucket agents by Waiting/Error/Running/Planning/Done/Idle) |
| `c` | Collapse / expand the group under the cursor |
| `Space` | Toggle favorite on the selected agent |
| `f` | Filter to favorites only |
| `g` | Toggle 2×2 grid view |
| `a` | Toggle the last-activity line under each agent |
| `r` | Refresh agent list |
| `q` | Quit |

Collapsed groups and favorites persist across restarts (`~/.config/agent-monitor/collapsed.json`, `favorites.json`). Group-by-status regroups the same agents by their live state — handy for answering "what's waiting on me right now?" across every project at once.

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

## Agent Groups

Organize agents into named groups by creating `~/.config/agent-monitor/groups.yaml`:

```yaml
groups:
  - name: Astronomy
    sessions: [astra, esc, erewhot, randomerewhon]
  - name: Web
    sessions: [webapp, frontend]
```

- Groups are displayed in config order with colored headers
- Agents not matching any group appear in an "Other" section at the bottom
- If the config file is missing or malformed, agents display in a flat alphabetical list
- Groups cycle through purple/violet header colors; "Other" uses a dimmer blue-gray

## Task Backends (Kanban board)

The web Kanban board (`http://localhost:8070`) can pull tasks from multiple
project-management backends at once — **Nous**, **GitHub Issues** (public or
Enterprise), and **git-bug** (per-repo) — configured in
`~/.config/agent-monitor/backends.yaml`.

A **project** is a swim lane and declares one or more backends. Cards are badged
by source (`nous` / `gh` / `bug`), link out to the issue/page, and moving a card
between columns writes the change back to its tracker (close/reopen an issue,
`git bug status`, Nous tags + log).

```yaml
nous:                          # optional global Nous connection
  url: http://localhost:7667
  notebook: Forge
  tag: task
  api_key: rw:…                # or set NOUS_API_KEY (Nous now requires a key)
  import_all: true             # auto-show every Nous project not listed below

projects:
  - name: ProjectA             # → GitHub only
    backends:
      - github: { host: github.com, repo: erewhon/projectA }

  - name: ProjectB             # → git-bug only
    backends:
      - git-bug: { repo: ~/Projects/projectB }

  - name: AgentMonitor         # → one lane, two trackers merged
    nous_project: "Agent Monitor"   # only if the Nous name differs from `name`
    backends:
      - nous: {}
      - github: { repo: erewhon/agent-monitor }
```

- **Granularity:** a listed project's `backends` are authoritative for its lane,
  so a repo fully moved to git-bug shows only git-bug, while a mid-migration repo
  can merge Nous + GitHub. Unlisted Nous projects keep auto-appearing
  (`import_all: true`).
- **GitHub auth:** per backend, `token:` → `token_cmd:` (default `gh auth token`)
  → `GITHUB_TOKEN` / `GH_TOKEN`. Enterprise via `host:`.
- **Optional per-backend knobs:** `writable` (default true), `poll_interval`
  (default 60s), and GitHub/git-bug `active_label` / `needs_input_label` to map
  labels to the Active / Needs-input columns.
- **Back-compat:** if `backends.yaml` is absent, a legacy
  `~/.config/agent-monitor/nous.yaml` is used as a single Nous backend.

## Status Indicators

| Symbol | Status | Description |
|--------|--------|-------------|
| `⠋` (green, animated) | Running | Agent is actively working (animated Braille spinner) |
| `◐` (yellow) | Waiting | Agent needs user input (permission prompt) |
| `○` (gray) | Idle | Agent is ready for a new command |
| `✕` (red) | Error | Agent encountered an error |

## How It Works

1. **Outer tmux session**: Runs on a separate socket (`-L agent-monitor`) with its own config and prefix (`C-a`) to avoid conflicts with your regular tmux
2. **Agent detection**: Scans all panes in the default tmux socket for processes named `claude`, `opencode`, or `crush`. Falls back to content probing for agents running inside wrappers (e.g. dx, containers)
3. **Status detection**: Captures pane content to detect state based on each agent type's UI patterns
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
| `groups.yaml` | `~/.config/agent-monitor/` | Agent grouping config (optional) |
| `backends.yaml` | `~/.config/agent-monitor/` | Kanban task backends: Nous / GitHub / git-bug (optional) |

## Requirements

- Go 1.21+
- tmux
- One or more coding agents running in tmux sessions (Claude Code, OpenCode, or Crush)
