package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type gitbugSource struct {
	group    string // configured swim-lane name
	repoPath string // filesystem path to a git-bug-enabled repo
	base     string // repo basename, for source-id qualification
	writable bool
	poll     time.Duration

	activeLabel string
}

func newGitbugSource(cfg *gitbugBackendCfg, group string) *gitbugSource {
	path := expandHome(cfg.Repo)
	if path == "" {
		return nil
	}
	poll := time.Duration(cfg.Poll) * time.Second
	if poll <= 0 {
		poll = 60 * time.Second
	}
	return &gitbugSource{
		group:       group,
		repoPath:    path,
		base:        filepath.Base(path),
		writable:    boolOr(cfg.Writable, true),
		poll:        poll,
		activeLabel: cfg.ActiveLabel,
	}
}

func (s *gitbugSource) Name() string                { return "git-bug:" + s.repoPath }
func (s *gitbugSource) Kind() string                { return "git-bug" }
func (s *gitbugSource) Writable() bool              { return s.writable }
func (s *gitbugSource) PollInterval() time.Duration { return s.poll }

// git runs a git-bug subcommand inside the repo.
func (s *gitbugSource) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git-bug", args...)
	cmd.Dir = s.repoPath
	return cmd.Output()
}

// gitbugBug mirrors the fields we use from `git-bug bug --format json`.
type gitbugBug struct {
	ID     string   `json:"id"`
	Status string   `json:"status"` // "open" | "closed"
	Title  string   `json:"title"`
	Labels []string `json:"labels"`
}

func (s *gitbugSource) columnFor(b gitbugBug) TaskColumn {
	if b.Status == "closed" {
		return ColumnDone
	}
	if s.activeLabel != "" {
		for _, l := range b.Labels {
			if strings.EqualFold(l, s.activeLabel) {
				return ColumnActive
			}
		}
	}
	return ColumnBacklog
}

func (s *gitbugSource) Fetch() ([]SourceTask, error) {
	data, err := s.git("bug", "--format", "json", "--status", "open,closed")
	if err != nil {
		return nil, fmt.Errorf("git-bug ls: %w", err)
	}
	var bugs []gitbugBug
	if err := json.Unmarshal(data, &bugs); err != nil {
		return nil, fmt.Errorf("git-bug json: %w", err)
	}
	out := make([]SourceTask, 0, len(bugs))
	for _, b := range bugs {
		out = append(out, SourceTask{
			SourceID: s.base + ":" + b.ID,
			Title:    b.Title,
			Project:  s.group,
			Status:   b.Status,
			Column:   s.columnFor(b),
		})
	}
	return out, nil
}

// bugID recovers the git-bug id from a "<base>:<id>" SourceID.
func (s *gitbugSource) bugID(sourceID string) string {
	if _, id, ok := strings.Cut(sourceID, ":"); ok {
		return id
	}
	return sourceID
}

func (s *gitbugSource) Push(t SourceTask, col TaskColumn) error {
	id := s.bugID(t.SourceID)
	verb := "open"
	if col == ColumnDone {
		verb = "close"
	}
	if _, err := s.git("bug", "status", verb, id); err != nil {
		return fmt.Errorf("git-bug status %s: %w", verb, err)
	}
	if s.activeLabel != "" {
		if col == ColumnActive {
			s.git("bug", "label", "new", id, s.activeLabel)
		} else {
			s.git("bug", "label", "rm", id, s.activeLabel)
		}
	}
	return nil
}
