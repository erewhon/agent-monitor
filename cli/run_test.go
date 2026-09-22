package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStderr runs fn with os.Stderr redirected and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}

func TestRunUnknownFlagReturnsUsageError(t *testing.T) {
	var err error
	out := captureStderr(t, func() {
		err = Run(context.Background(), []string{"-bogus"})
	})

	var usageErr *UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("Run(-bogus) returned %T (%v), want *UsageError", err, err)
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Run(-bogus) must not be flag.ErrHelp")
	}
	if n := strings.Count(out, "flag provided but not defined: -bogus"); n != 1 {
		t.Errorf("error line printed %d times, want exactly once; stderr:\n%s", n, out)
	}
	if n := strings.Count(out, "Usage of agent-monitor:"); n != 1 {
		t.Errorf("usage printed %d times, want exactly once; stderr:\n%s", n, out)
	}
}

func TestRunHelpReturnsErrHelp(t *testing.T) {
	var err error
	out := captureStderr(t, func() {
		err = Run(context.Background(), []string{"--help"})
	})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Run(--help) returned %v, want flag.ErrHelp", err)
	}
	var usageErr *UsageError
	if errors.As(err, &usageErr) {
		t.Fatalf("Run(--help) must not be a *UsageError")
	}
	if n := strings.Count(out, "Usage of agent-monitor:"); n != 1 {
		t.Errorf("usage printed %d times, want exactly once; stderr:\n%s", n, out)
	}
}

func TestWebhookStoreKeepsSessionIDPastTTL(t *testing.T) {
	ws := newWebhookStore()
	ws.Set(WebhookState{Session: "proj", Status: "running", SessionID: "0d3e4b2a-1111-4c2d-9e8f-000000000001", Timestamp: time.Now().Add(-2 * webhookTTL)})
	if _, ok := ws.Get("proj"); ok {
		t.Fatal("expired state should not be returned")
	}
	if got := ws.SessionID("proj"); got != "0d3e4b2a-1111-4c2d-9e8f-000000000001" {
		t.Fatalf("SessionID after TTL = %q", got)
	}
	// A later hook without the id must not erase it; one with a new id replaces it.
	ws.Set(WebhookState{Session: "proj", Status: "idle"})
	if ws.SessionID("proj") == "" {
		t.Fatal("state without session_id erased the remembered id")
	}
	ws.Set(WebhookState{Session: "proj", Status: "running", SessionID: "new"})
	if ws.SessionID("proj") != "new" {
		t.Fatal("new session_id not recorded")
	}
	if ws.SessionID("other") != "" {
		t.Fatal("unknown session should have no id")
	}
}

func TestAPIAgentSessionIDIsOptional(t *testing.T) {
	a := toAPIAgent(Agent{Name: "x", Session: "proj", Type: AgentClaude})
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "session_id") {
		t.Fatalf("session_id must be omitted when unknown: %s", b)
	}
	a.SessionID = "abc"
	b, _ = json.Marshal(a)
	if !strings.Contains(string(b), `"session_id":"abc"`) {
		t.Fatalf("session_id missing: %s", b)
	}
}
