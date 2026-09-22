package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
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
