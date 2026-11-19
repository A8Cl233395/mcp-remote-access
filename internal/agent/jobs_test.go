package agent

import (
	"strings"
	"testing"
	"time"
)

func TestReadJobWaitForExit(t *testing.T) {
	reg := NewJobRegistry()

	// Create a synthetic running job that emits output before exiting.
	rec := &JobRecord{
		ID:           "1",
		Command:      "test-cmd",
		StartedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		LastOutputAt: time.Now(),
		Status:       JobStatusRunning,
		Promoted:     true,
		notifyCh:     make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	reg.mu.Lock()
	reg.records["1"] = rec
	reg.mu.Unlock()

	go func() {
		time.Sleep(50 * time.Millisecond)
		rec.mu.Lock()
		rec.buf.WriteString("first line\n")
		rec.LastOutputAt = time.Now()
		rec.mu.Unlock()
		select {
		case rec.notifyCh <- struct{}{}:
		default:
		}

		time.Sleep(120 * time.Millisecond)
		rec.mu.Lock()
		rec.buf.WriteString("second line\n")
		code := 0
		now := time.Now()
		rec.ExitCode = &code
		rec.Status = JobStatusExited
		rec.CompletedAt = &now
		rec.LastOutputAt = now
		rec.mu.Unlock()
		close(rec.doneCh)
	}()

	// wait_for_exit=true should NOT return on "first line\n", it should wait for exit and return both lines.
	waitMs := 2000
	content, meta, err := reg.ReadJob("1", nil, &waitMs, true)
	if err != nil {
		t.Fatalf("ReadJob(waitForExit=true) failed: %v", err)
	}
	if meta["status"] != string(JobStatusExited) {
		t.Fatalf("expected status=%q, got %v", JobStatusExited, meta["status"])
	}
	if !strings.Contains(content, "first line") || !strings.Contains(content, "second line") {
		t.Fatalf("expected both lines in exit output, got:\n%s", content)
	}
}

func TestReadJobWaitForExitTimeoutWhileRunning(t *testing.T) {
	reg := NewJobRegistry()

	rec := &JobRecord{
		ID:           "2",
		Command:      "slow-cmd",
		StartedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		LastOutputAt: time.Now(),
		Status:       JobStatusRunning,
		Promoted:     true,
		notifyCh:     make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
	reg.mu.Lock()
	reg.records["2"] = rec
	reg.mu.Unlock()

	defer close(rec.doneCh)

	go func() {
		time.Sleep(30 * time.Millisecond)
		rec.mu.Lock()
		rec.buf.WriteString("partial output\n")
		rec.LastOutputAt = time.Now()
		rec.mu.Unlock()
		select {
		case rec.notifyCh <- struct{}{}:
		default:
		}
	}()

	waitMs := 150
	start := time.Now()
	content, meta, err := reg.ReadJob("2", nil, &waitMs, true)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ReadJob(waitForExit=true) failed: %v", err)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("expected waitForExit=true to wait near timeout (150ms), returned in %v", elapsed)
	}
	if meta["status"] != string(JobStatusRunning) {
		t.Fatalf("expected status=%q, got %v", JobStatusRunning, meta["status"])
	}
	if !strings.Contains(content, "partial output") {
		t.Fatalf("expected partial output after timeout, got:\n%s", content)
	}
}
