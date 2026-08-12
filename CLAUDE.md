# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build the binary
go build -o agent-monitor .

# Install all components
cp agent-monitor agent-monitor-session focus-agent-monitor ~/.local/bin/
cp tmux-outer.conf ~/.config/agent-monitor-tmux.conf

# Launch the full monitor session (outer tmux + TUI + preview pane)
agent-monitor-session

# Run TUI standalone (no outer tmux integration)
agent-monitor --no-attach

# List agents non-interactively (for scripts/status bars)
agent-monitor --list
```

## Architecture

Go TUI in `main.go` using the [Bubble Tea](https://github.com/charmbracelet/bubbletea) framework (Elm architecture: Model/Update/View). The Kanban task-backend layer lives in separate `package main` files: `backends.go` (the `TaskSource` interface, `backends.yaml` loader, generic sync loop, and Nous source) plus `backend_github.go` and `backend_gitbug.go`.

**Task backends:** The web Kanban board reads/writes a local JSON `TaskStore` (`~/.config/agent-monitor/tasks.json`), which is fed by one goroutine per configured `TaskSource` (`startBackendSyncLoop`). Each `Task` carries `Source` (unique routing key + badge), `SourceID`, and `URL`. `backends.yaml` is project-centric: a project = one swim lane declaring one or more backends (`nous` / `github` / `git-bug`); a listed project's backends are authoritative for its lane, while unlisted Nous projects auto-appear when `import_all: true`. If `backends.yaml` is absent, the legacy `nous.yaml` is used as a single Nous backend. The reconcile loop is **push-first** (board→backend before the authoritative pull) so board moves aren't reverted mid-tick. The Nous server requires a bearer token (`api_key` / `NOUS_API_KEY`, falling back to the daemon key file via `discoverNousAPIKey`).

**Backend failures never touch stderr.** The TUI owns the terminal, so a stray `fmt.Fprintf(os.Stderr, …)` from a sync goroutine tears a hole through the rendered frame. Failures go to the `backendHealth` registry (`recordBackendErr` / `clearBackendErr`, keyed `source|op` so a healthy fetch doesn't clear a standing push failure) — the TUI footer renders them via `renderBackendIssues`, and `backendLogf` appends to `~/.local/state/agent-monitor/agent-monitor.log`, echoing to stderr only when `tuiActive` is false (`--web-only`, `--list`). Every Nous read goes through `NousClient.getJSON`, which checks HTTP status and the envelope's `error` field: skipping that check is what made a 401 surface as the misleading `notebook "Forge" not found`.

**Panel widths.** Nothing may exceed `Model.panelContentWidth()` (terminal minus border and padding) — lipgloss wraps an over-wide row, which desyncs every line below it from its cursor index. Agent names are fitted by `fitName` (measured in terminal cells, around the already-rendered symbol/badge/suffix fragments), other lines by `fitLine`; both use `truncateCells`, the display-width-aware counterpart of the byte-based `truncate`. `render_test.go` asserts no rendered line overflows.

**Core types:**
- `Agent` — represents a coding agent instance (Claude Code, OpenCode, or Crush) detected in a tmux pane (session:window.pane targeting)
- `AgentType` — enum: Claude/OpenCode/Crush/Unknown, determines which detection patterns to use
- `Model` — Bubble Tea model holding agent list, cursor position, groups, flatAgents, and outer tmux socket reference
- `AgentStatus` — enum: Running/Planning/Waiting/Idle/Error, detected via pattern matching on captured tmux pane content
- `WaitReason` — orthogonal refinement of `StatusWaiting`: `WaitApproval` (blocked on a tool/permission gate) vs `WaitInput` (blocked on a question). Kept separate from `AgentStatus` so the API's `status` field stays `"waiting"` and the sub-state rides along in an additive `wait_reason` field
- `Config` / `GroupConfig` — YAML-mapped structs for agent grouping config
- `Group` — `{Name string, Agents []Agent}` for grouped rendering

**Config file:** `~/.config/agent-monitor/groups.yaml` — optional YAML file that defines named groups of agents by session name. If missing or malformed, agents display in a flat list.

**Agent detection flow:** `detectAgents()` calls `tmux list-panes -a` on the default socket, matches pane commands against `claude`/`opencode`/`crush`, then falls back to content probes (`looksLikeClaude`/`looksLikeCrush`/`looksLikeOpenCode`) for wrapped processes. `detectAgentStatus()` dispatches to type-specific detectors (`detectClaudeStatus`/`detectCrushStatus`/`detectOpenCodeStatus`) that match against each tool's UI patterns and return a `Detection{Status, Wait, Line}`.

Each detector is a thin `capturePane` + `classifyXStatus(content string) Detection` pair — the `classify*` half is pure, so pane fixtures are testable without tmux (`detection_test.go`). Approval markers are line-start-anchored via the shared `paneChrome` prefix so words like "allow" in prose or stale scrollback don't read as a permission prompt.

**Waiting sub-states:** hook state posted to `POST /api/webhook` is authoritative and overrides the pane heuristic while fresh (`webhookTTL`); `WebhookState.resolvedWait()` resolves the reason from an explicit `wait_reason`, a compound `waiting-approval` status, or a `hook_event` name. Transition/debounce tracking keys on `agentState{Status, Wait}` rather than status alone, so an input→approval escalation notifies.

**Nested tmux design:** The monitor runs inside an "outer" tmux session on a separate socket (`-L agent-monitor`) with prefix `C-a`, so it doesn't conflict with users' regular tmux (`C-b`). The outer session has two panes: left = TUI, right = live attach to selected agent's tmux session via `respawn-pane`.

**Shell scripts:**
- `agent-monitor-session` — launcher that creates the outer tmux session with split layout
- `agent-monitor-placeholder` — placeholder display for the right pane before an agent is selected
- `focus-agent-monitor` — helper to return focus from inner tmux to the monitor pane

## Task Management

Task specs and feature specs live in the **Forge** notebook in Nous. When working on a task:

- Use `mcp__nous__get_page` to read the task spec from Forge (e.g., "Task: Kanban Board Web UI")
- To check task status and dependencies, use the **targeted query tools** (NOT `get_database`, which is too large):
  - `mcp__nous__task_summary` — cheapest: task counts by project/status/feature
  - `mcp__nous__query_tasks` — filtered queries with compact rows (by project, feature, status, phase, priority, blocked state)
  - `mcp__nous__get_feature_tasks` — tasks for a project/feature in dependency-resolved execution order
- Update task status via `mcp__nous__update_task_status` — pass the task name; it looks up the row, updates Status + Completed date, syncs page tags, fires the webhook, and optionally appends implementation notes via `notes=`. Same call accepts `external_ref`, `execution_mode`, `model_tier`, `estimate`, `complexity`, `task_type`, `max_files`, `requires_tests`. Avoid `mcp__nous__update_database_rows` for tasks — it's the slow path that requires a row-UUID lookup.
- Feature pages in Forge contain the full context: data model, API contracts, edge cases, test plans

Do NOT use `mcp__nous__get_database` on the Project Tasks database — it returns too much data. Use the targeted query tools above.

Do NOT create ad-hoc task tracking internally — all task state lives in Forge.

## Version Control

This repo uses **jj** (Jujutsu), not git.
