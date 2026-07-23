package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v66/github"
)

type githubSource struct {
	group    string // configured swim-lane name
	host     string // "github.com" or an enterprise host
	owner    string
	repo     string
	token    string
	tokenCmd string
	writable bool
	poll     time.Duration

	activeLabel string
	needsLabel  string

	mu     sync.Mutex
	client *github.Client
	inited bool
}

func newGithubSource(cfg *githubBackendCfg, group string) *githubSource {
	owner, repo, ok := strings.Cut(cfg.Repo, "/")
	if !ok || owner == "" || repo == "" {
		fmt.Fprintf(os.Stderr, "agent-monitor: github backend for %q has invalid repo %q (want owner/repo)\n", group, cfg.Repo)
		return nil
	}
	host := cfg.Host
	if host == "" {
		host = "github.com"
	}
	tokenCmd := cfg.TokenCmd
	if tokenCmd == "" && cfg.Token == "" {
		tokenCmd = "gh auth token"
	}
	poll := time.Duration(cfg.Poll) * time.Second
	if poll <= 0 {
		poll = 60 * time.Second
	}
	return &githubSource{
		group:       group,
		host:        host,
		owner:       owner,
		repo:        repo,
		token:       cfg.Token,
		tokenCmd:    tokenCmd,
		writable:    boolOr(cfg.Writable, true),
		poll:        poll,
		activeLabel: cfg.ActiveLabel,
		needsLabel:  cfg.NeedsInputLabel,
	}
}

func (s *githubSource) Name() string                { return "github:" + s.host + "/" + s.owner + "/" + s.repo }
func (s *githubSource) Kind() string                { return "github" }
func (s *githubSource) Writable() bool              { return s.writable }
func (s *githubSource) PollInterval() time.Duration { return s.poll }

func (s *githubSource) ensure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inited {
		return nil
	}
	tok := s.token
	if tok == "" && s.tokenCmd != "" {
		if out, err := exec.Command("sh", "-c", s.tokenCmd).Output(); err == nil {
			tok = strings.TrimSpace(string(out))
		}
	}
	if tok == "" {
		tok = os.Getenv("GITHUB_TOKEN")
	}
	if tok == "" {
		tok = os.Getenv("GH_TOKEN")
	}
	c := github.NewClient(nil).WithAuthToken(tok)
	if s.host != "" && s.host != "github.com" {
		ec, err := c.WithEnterpriseURLs("https://"+s.host+"/api/v3/", "https://"+s.host+"/api/uploads/")
		if err != nil {
			return fmt.Errorf("enterprise URL: %w", err)
		}
		c = ec
	}
	s.client = c
	s.inited = true
	return nil
}

func (s *githubSource) columnFor(is *github.Issue) TaskColumn {
	if is.GetState() == "closed" {
		return ColumnDone
	}
	for _, l := range is.Labels {
		name := strings.ToLower(l.GetName())
		if s.needsLabel != "" && name == strings.ToLower(s.needsLabel) {
			return ColumnNeedsInput
		}
		if s.activeLabel != "" && name == strings.ToLower(s.activeLabel) {
			return ColumnActive
		}
	}
	return ColumnBacklog
}

func (s *githubSource) sourceID(number int) string {
	return fmt.Sprintf("%s/%s#%d", s.owner, s.repo, number)
}

func (s *githubSource) Fetch() ([]SourceTask, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	var out []SourceTask
	// All open issues, plus issues closed in the last 30 days (so Done doesn't
	// accumulate unbounded history).
	since := time.Now().Add(-30 * 24 * time.Hour)
	for _, phase := range []struct {
		state string
		since time.Time
	}{{"open", time.Time{}}, {"closed", since}} {
		opt := &github.IssueListByRepoOptions{
			State:       phase.state,
			Sort:        "updated",
			ListOptions: github.ListOptions{PerPage: 100},
		}
		if !phase.since.IsZero() {
			opt.Since = phase.since
		}
		for {
			issues, resp, err := s.client.Issues.ListByRepo(ctx, s.owner, s.repo, opt)
			if err != nil {
				return nil, err
			}
			for _, is := range issues {
				if is.IsPullRequest() {
					continue
				}
				out = append(out, SourceTask{
					SourceID: s.sourceID(is.GetNumber()),
					Title:    is.GetTitle(),
					Project:  s.group,
					Status:   is.GetState(),
					Column:   s.columnFor(is),
					URL:      is.GetHTMLURL(),
				})
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	return out, nil
}

// issueNumber parses the trailing "#123" of a SourceID.
func issueNumber(sourceID string) (int, error) {
	_, num, ok := strings.Cut(sourceID, "#")
	if !ok {
		return 0, fmt.Errorf("bad github source id %q", sourceID)
	}
	return strconv.Atoi(num)
}

func (s *githubSource) Push(t SourceTask, col TaskColumn) error {
	if err := s.ensure(); err != nil {
		return err
	}
	num, err := issueNumber(t.SourceID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	state := "open"
	if col == ColumnDone {
		state = "closed"
	}
	if _, _, err := s.client.Issues.Edit(ctx, s.owner, s.repo, num, &github.IssueRequest{State: &state}); err != nil {
		return err
	}

	// Best-effort status labels (only when configured).
	if s.activeLabel != "" || s.needsLabel != "" {
		add, remove := "", []string{}
		switch col {
		case ColumnActive:
			add = s.activeLabel
			if s.needsLabel != "" {
				remove = append(remove, s.needsLabel)
			}
		case ColumnNeedsInput:
			add = s.needsLabel
			if s.activeLabel != "" {
				remove = append(remove, s.activeLabel)
			}
		default:
			if s.activeLabel != "" {
				remove = append(remove, s.activeLabel)
			}
			if s.needsLabel != "" {
				remove = append(remove, s.needsLabel)
			}
		}
		if add != "" {
			s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, num, []string{add})
		}
		for _, l := range remove {
			s.client.Issues.RemoveLabelForIssue(ctx, s.owner, s.repo, num, l)
		}
	}
	return nil
}
