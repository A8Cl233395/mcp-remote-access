package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	DefaultInlineWaitMs      = 30_000
	ExplicitBackgroundReadMs = 3_000
	MaxTimeoutMs             = 15 * 60_000
	DefaultJobReadWaitMs     = 5_000
	MaxJobReadWaitMs         = 60_000
	StallAfterMs             = 30_000
	ExitSettleMs             = 150
	MaxJobs                  = 16
	LogMaxBytes              = 64 * 1024 * 1024
	LogRetain                = 20
	DefaultMaxReadLines      = 2000
	DefaultMaxReadBytes      = 50 * 1024
)

type JobStatus string

const (
	JobStatusRunning JobStatus = "running"
	JobStatusExited  JobStatus = "exited"
	JobStatusKilled  JobStatus = "killed"
)

type JobRecord struct {
	mu           sync.Mutex
	ID           string
	PID          int
	Command      string
	Workdir      string
	Shell        string
	StartedAt    time.Time
	UpdatedAt    time.Time
	LastOutputAt time.Time
	CompletedAt  *time.Time
	Status       JobStatus
	ExitCode     *int
	KillReason   string // "", "user", "log_limit"
	SpawnError   string
	Promoted     bool
	Origin       string // "auto" | "explicit"

	buf      bytes.Buffer
	cmd      *exec.Cmd
	doneCh   chan struct{}
	notifyCh chan struct{}
}

func (r *JobRecord) writeOutput(p []byte) (int, error) {
	r.mu.Lock()
	now := time.Now()
	r.LastOutputAt = now
	r.UpdatedAt = now
	var limitHit bool
	if r.buf.Len()+len(p) > LogMaxBytes {
		remain := LogMaxBytes - r.buf.Len()
		if remain > 0 {
			r.buf.Write(p[:remain])
		}
		limitHit = true
	} else {
		r.buf.Write(p)
	}
	select {
	case r.notifyCh <- struct{}{}:
	default:
	}
	r.mu.Unlock()

	if limitHit {
		KillProcessTree(r.PID)
	}
	return len(p), nil
}

type jobOutputWriter struct {
	rec *JobRecord
}

func (w jobOutputWriter) Write(p []byte) (int, error) {
	return w.rec.writeOutput(p)
}

type JobRegistry struct {
	mu      sync.Mutex
	records map[string]*JobRecord
	cursors map[string]int
	nextID  int
}

func NewJobRegistry() *JobRegistry {
	return &JobRegistry{
		records: make(map[string]*JobRecord),
		cursors: make(map[string]int),
		nextID:  1,
	}
}

func (reg *JobRegistry) RunningCount() int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	count := 0
	for _, r := range reg.records {
		r.mu.Lock()
		if r.Status == JobStatusRunning {
			count++
		}
		r.mu.Unlock()
	}
	return count
}

func (reg *JobRegistry) ExecuteShell(command, workdir, shellPath string, timeoutMs *int, onStateChange func()) (string, map[string]any, error) {
	if strings.TrimSpace(command) == "" {
		return "", nil, fmt.Errorf("The shell tool was called with invalid arguments: `command` must be a non-empty string.")
	}
	if reg.RunningCount() >= MaxJobs {
		return "", nil, fmt.Errorf("Background job limit reached (%d/%d). Run job_list and job_kill one of the running jobs first.", MaxJobs, MaxJobs)
	}

	waitMs := DefaultInlineWaitMs
	if timeoutMs != nil {
		if *timeoutMs < 0 {
			return "", nil, fmt.Errorf("The shell tool was called with invalid arguments: `timeout` must be a non-negative number.")
		}
		if *timeoutMs == 0 {
			waitMs = 0
		} else if *timeoutMs > MaxTimeoutMs {
			waitMs = MaxTimeoutMs
		} else {
			waitMs = *timeoutMs
		}
	}

	now := time.Now()

	rec := &JobRecord{
		Command:      command,
		Workdir:      workdir,
		Shell:        shellPath,
		StartedAt:    now,
		UpdatedAt:    now,
		LastOutputAt: now,
		Status:       JobStatusRunning,
		Origin:       "auto",
		doneCh:       make(chan struct{}),
		notifyCh:     make(chan struct{}, 1),
	}

	cmd := BuildShellCommand(shellPath, command, workdir)
	writer := jobOutputWriter{rec: rec}
	cmd.Stdout = writer
	cmd.Stderr = writer
	rec.cmd = cmd

	if err := cmd.Start(); err != nil {
		nowEnd := time.Now()
		exitCode := 1
		rec.SpawnError = err.Error()
		rec.Status = JobStatusExited
		rec.ExitCode = &exitCode
		rec.CompletedAt = &nowEnd
		close(rec.doneCh)
		return rec.SpawnError, map[string]any{"exit": exitCode, "status": "completed"}, nil
	}
	if cmd.Process != nil {
		rec.PID = cmd.Process.Pid
	}

	go func() {
		err := cmd.Wait()
		time.Sleep(ExitSettleMs * time.Millisecond)
		rec.mu.Lock()
		nowEnd := time.Now()
		rec.UpdatedAt = nowEnd
		if rec.CompletedAt == nil {
			rec.CompletedAt = &nowEnd
		}
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		rec.ExitCode = &code
		if rec.Status == JobStatusRunning {
			rec.Status = JobStatusExited
		}
		select {
		case rec.notifyCh <- struct{}{}:
		default:
		}
		promoted := rec.Promoted
		rec.mu.Unlock()
		close(rec.doneCh)
		if promoted {
			reg.prune()
			if onStateChange != nil {
				onStateChange()
			}
		}
	}()

	// Explicit background start: timeout == 0
	if waitMs == 0 {
		reg.promote(rec, "explicit")
		if onStateChange != nil {
			onStateChange()
		}
		content, meta, err := reg.ReadJob(rec.ID, nil, intPtr(ExplicitBackgroundReadMs))
		if err == nil && content != "" {
			return content, meta, nil
		}
		return reg.backgroundReceipt(rec)
	}

	timer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-rec.doneCh:
		// Completed within foreground timeout
		rec.mu.Lock()
		raw := rec.buf.String()
		exitCode := 0
		if rec.ExitCode != nil {
			exitCode = *rec.ExitCode
		}
		spawnErr := rec.SpawnError
		rec.mu.Unlock()

		text := raw
		if text == "" {
			text = spawnErr
		}
		if strings.TrimSpace(text) == "" {
			text = "(no output)"
		} else {
			text, _ = truncateOutput(text, DefaultMaxReadLines, DefaultMaxReadBytes)
		}
		return text, map[string]any{"exit": exitCode, "status": "completed"}, nil

	case <-timer.C:
		// Timed out -> promote to background job
		reg.promote(rec, "auto")
		if onStateChange != nil {
			onStateChange()
		}
		return reg.backgroundReceipt(rec)
	}
}

func (reg *JobRegistry) promote(rec *JobRecord, origin string) {
	reg.mu.Lock()
	id := fmt.Sprintf("%d", reg.nextID)
	reg.nextID++
	rec.mu.Lock()
	rec.ID = id
	rec.Promoted = true
	rec.Origin = origin
	rec.UpdatedAt = time.Now()
	rec.mu.Unlock()
	reg.records[id] = rec
	reg.mu.Unlock()
}

func (reg *JobRegistry) KillAll() {
	reg.mu.Lock()
	ids := make([]string, 0, len(reg.records))
	for id := range reg.records {
		ids = append(ids, id)
	}
	reg.mu.Unlock()
	for _, id := range ids {
		_, _, _ = reg.KillJob(id, nil)
	}
}

func (reg *JobRegistry) backgroundReceipt(rec *JobRecord) (string, map[string]any, error) {
	chunk, nextOffset := reg.readChunk(rec, 0, DefaultMaxReadBytes-4096, DefaultMaxReadLines-16)
	reg.setCursor(rec.ID, nextOffset)

	rec.mu.Lock()
	status := rec.Status
	spawnErr := rec.SpawnError
	rec.mu.Unlock()

	body := strings.TrimRight(chunk, " \t\r\n")
	if body == "" {
		body = strings.TrimRight(spawnErr, " \t\r\n")
	}
	if body == "" {
		body = "(no output)"
	}

	receipt := fmt.Sprintf("<shell_metadata>\nCommand moved to the background (job #%s).\n</shell_metadata>\n\n%s", rec.ID, body)
	meta := map[string]any{
		"jobId":       rec.ID,
		"background":  true,
		"status":      string(status),
		"next_offset": nextOffset,
	}
	return receipt, meta, nil
}

func (reg *JobRegistry) ListJobs() (string, map[string]any) {
	reg.mu.Lock()
	entries := make([]*JobRecord, 0, len(reg.records))
	for _, r := range reg.records {
		entries = append(entries, r)
	}
	reg.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].StartedAt.Before(entries[j].StartedAt)
	})

	running := 0
	for _, r := range entries {
		r.mu.Lock()
		if r.Status == JobStatusRunning {
			running++
		}
		r.mu.Unlock()
	}

	if len(entries) == 0 {
		return "No background jobs in this session.", map[string]any{
			"count":   0,
			"running": 0,
			"max":     MaxJobs,
		}
	}

	lines := []string{fmt.Sprintf("Background jobs in this session (%d/%d):", running, MaxJobs)}
	now := time.Now()
	for _, r := range entries {
		r.mu.Lock()
		end := now
		if r.CompletedAt != nil {
			end = *r.CompletedAt
		}
		elapsed := FormatElapsed(end.Sub(r.StartedAt))
		cmdDisplay := displayCommand(r.Command, 60)
		if r.Status == JobStatusRunning {
			lines = append(lines, fmt.Sprintf("  #%s  running  %s   %s", r.ID, elapsed, cmdDisplay))
		} else {
			codeStr := "?"
			if r.ExitCode != nil {
				codeStr = fmt.Sprintf("%d", *r.ExitCode)
			}
			lines = append(lines, fmt.Sprintf("  #%s  %s  %s   %s   (code %s)", r.ID, r.Status, elapsed, cmdDisplay, codeStr))
		}
		r.mu.Unlock()
	}

	return strings.Join(lines, "\n"), map[string]any{
		"count":   len(entries),
		"running": running,
		"max":     MaxJobs,
	}
}

func (reg *JobRegistry) ReadJob(id string, offset *int, timeoutMs *int) (string, map[string]any, error) {
	id = normalizeJobID(id)
	rec := reg.get(id)
	if rec == nil {
		return "", nil, fmt.Errorf("Unknown job id %s. Run job_list to see tracked jobs.", id)
	}

	start := reg.getCursor(id)
	if offset != nil && *offset >= 0 {
		start = *offset
	}

	waitMs := DefaultJobReadWaitMs
	if timeoutMs != nil && *timeoutMs >= 0 {
		waitMs = *timeoutMs
		if waitMs > MaxJobReadWaitMs {
			waitMs = MaxJobReadWaitMs
		}
	}

	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)
	chunkMaxBytes := DefaultMaxReadBytes - 4096
	chunkMaxLines := DefaultMaxReadLines - 16

	for {
		chunk, nextOffset := reg.readChunk(rec, start, chunkMaxBytes, chunkMaxLines)
		rec.mu.Lock()
		running := rec.Status == JobStatusRunning
		silentFor := time.Since(rec.LastOutputAt)
		totalBytes := rec.buf.Len()
		status := rec.Status
		exitCode := rec.ExitCode
		startedAt := rec.StartedAt
		completedAt := rec.CompletedAt
		spawnErr := rec.SpawnError
		rec.mu.Unlock()

		if len(chunk) > 0 {
			reg.setCursor(id, nextOffset)
			if !running {
				pending := totalBytes - nextOffset
				if pending < 0 {
					pending = 0
				}
				return formatExitReceipt(rec, chunk, pending, status == JobStatusKilled), map[string]any{
					"jobId":       id,
					"status":      string(status),
					"exit":        exitCode,
					"next_offset": nextOffset,
				}, nil
			}
			elapsed := time.Since(startedAt)
			head := fmt.Sprintf("job #%s running (%s)\nnext_offset: %d\n\n%s", id, FormatElapsed(elapsed), nextOffset, strings.TrimRight(chunk, " \t\r\n"))
			return head, map[string]any{
				"jobId":       id,
				"status":      string(status),
				"next_offset": nextOffset,
				"silentForMs": silentFor.Milliseconds(),
			}, nil
		}

		if !running {
			tailText := reg.jobTail(rec, 40, 8192)
			if tailText == "" {
				tailText = spawnErr
			}
			return formatExitReceipt(rec, tailText, 0, status == JobStatusKilled), map[string]any{
				"jobId":       id,
				"status":      string(status),
				"exit":        exitCode,
				"next_offset": start,
			}, nil
		}

		if time.Now().After(deadline) {
			if silentFor.Milliseconds() >= StallAfterMs {
				silentSec := int(silentFor.Seconds())
				return fmt.Sprintf("job #%s stalled - no output for %ds. job_kill it or wait longer.", id, silentSec), map[string]any{
					"jobId":       id,
					"status":      "stalled",
					"silentForMs": silentFor.Milliseconds(),
				}, nil
			}
			end := time.Now()
			if completedAt != nil {
				end = *completedAt
			}
			elapsed := end.Sub(startedAt)
			return fmt.Sprintf("job #%s running (%s)\nnext_offset: %d\n\n(no output)", id, FormatElapsed(elapsed), start), map[string]any{
				"jobId":       id,
				"status":      string(status),
				"next_offset": start,
				"silentForMs": silentFor.Milliseconds(),
			}, nil
		}

		remain := time.Until(deadline)
		if remain > 100*time.Millisecond {
			remain = 100 * time.Millisecond
		}
		select {
		case <-rec.notifyCh:
		case <-rec.doneCh:
		case <-time.After(remain):
		}
	}
}

func (reg *JobRegistry) KillJob(id string, onStateChange func()) (string, map[string]any, error) {
	id = normalizeJobID(id)
	rec := reg.get(id)
	if rec == nil {
		return "", nil, fmt.Errorf("Unknown job id %s. Run job_list to see tracked jobs.", id)
	}

	rec.mu.Lock()
	priorStatus := rec.Status
	pid := rec.PID
	if rec.Status == JobStatusRunning {
		rec.Status = JobStatusKilled
		rec.KillReason = "user"
		now := time.Now()
		rec.UpdatedAt = now
		rec.CompletedAt = &now
	}
	rec.mu.Unlock()

	if priorStatus == JobStatusRunning {
		KillProcessTree(pid)
		select {
		case <-rec.doneCh:
		case <-time.After(2 * time.Second):
		}
		if onStateChange != nil {
			onStateChange()
		}
	}

	start := reg.getCursor(id)
	unread, nextOffset := reg.readChunk(rec, start, DefaultMaxReadBytes-4096, DefaultMaxReadLines-16)
	if len(unread) > 0 {
		reg.setCursor(id, nextOffset)
	}

	rec.mu.Lock()
	totalBytes := rec.buf.Len()
	status := rec.Status
	exitCode := rec.ExitCode
	spawnErr := rec.SpawnError
	rec.mu.Unlock()

	pending := 0
	if len(unread) > 0 && totalBytes > nextOffset {
		pending = totalBytes - nextOffset
	}
	body := unread
	if body == "" {
		body = spawnErr
	}

	meta := map[string]any{
		"jobId":  id,
		"status": string(status),
		"exit":   exitCode,
	}

	if priorStatus == JobStatusRunning && status == JobStatusKilled {
		lines := []string{fmt.Sprintf("job #%s killed (%s).", id, KillTargetDescription(pid))}
		if pending > 0 {
			lines = append(lines, fmt.Sprintf("%d bytes unread - call job_read again.", pending))
		}
		trimmed := strings.TrimRight(body, " \t\r\n")
		if trimmed != "" {
			lines = append(lines, "", trimmed)
		}
		return strings.Join(lines, "\n"), meta, nil
	}

	return formatExitReceipt(rec, body, pending, status == JobStatusKilled), meta, nil
}

func formatExitReceipt(rec *JobRecord, body string, pendingBytes int, killed bool) string {
	rec.mu.Lock()
	end := time.Now()
	if rec.CompletedAt != nil {
		end = *rec.CompletedAt
	}
	elapsed := FormatElapsed(end.Sub(rec.StartedAt))
	codeStr := "?"
	if killed && rec.ExitCode == nil {
		codeStr = "-"
	} else if rec.ExitCode != nil {
		codeStr = fmt.Sprintf("%d", *rec.ExitCode)
	}
	reason := ""
	if rec.KillReason == "log_limit" {
		reason = " (log limit)"
	}
	id := rec.ID
	rec.mu.Unlock()

	var lines []string
	if killed {
		lines = append(lines, fmt.Sprintf("job #%s killed after %s%s", id, elapsed, reason))
	} else {
		lines = append(lines, fmt.Sprintf("job #%s exited with code %s after %s", id, codeStr, elapsed))
	}
	if pendingBytes > 0 {
		lines = append(lines, fmt.Sprintf("%d bytes unread - call job_read again.", pendingBytes))
	}
	trimmed := strings.TrimRight(body, " \t\r\n")
	if trimmed != "" {
		lines = append(lines, "", trimmed)
	}
	return strings.Join(lines, "\n")
}

func (reg *JobRegistry) readChunk(rec *JobRecord, offset, maxBytes, maxLines int) (string, int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	data := rec.buf.Bytes()
	total := len(data)
	if offset >= total || total == 0 {
		return "", offset
	}
	if offset < 0 {
		offset = 0
	}
	take := total - offset
	if take > maxBytes {
		take = maxBytes
	}
	slice := data[offset : offset+take]
	// Avoid splitting multi-byte UTF-8 rune at boundary
	for len(slice) > 0 && !utf8.Valid(slice) {
		slice = slice[:len(slice)-1]
	}
	text := string(slice)
	if maxLines > 0 {
		lines := 0
		for i := 0; i < len(text); i++ {
			if text[i] == '\n' {
				lines++
				if lines >= maxLines {
					text = text[:i+1]
					break
				}
			}
		}
	}
	advance := len(text)
	if advance == 0 && take > 0 {
		advance = take
	}
	return text, offset + advance
}

func (reg *JobRegistry) jobTail(rec *JobRecord, maxLines, maxBytes int) string {
	rec.mu.Lock()
	data := rec.buf.Bytes()
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	text := string(data)
	rec.mu.Unlock()

	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func (reg *JobRegistry) get(id string) *JobRecord {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.records[id]
}

func (reg *JobRegistry) getCursor(id string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.cursors[id]
}

func (reg *JobRegistry) setCursor(id string, offset int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if offset < 0 {
		offset = 0
	}
	reg.cursors[id] = offset
}

func (reg *JobRegistry) prune() {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	var finished []*JobRecord
	for _, r := range reg.records {
		r.mu.Lock()
		done := r.Status != JobStatusRunning
		r.mu.Unlock()
		if done {
			finished = append(finished, r)
		}
	}
	if len(finished) <= LogRetain {
		return
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].StartedAt.After(finished[j].StartedAt)
	})
	for _, r := range finished[LogRetain:] {
		delete(reg.records, r.ID)
		delete(reg.cursors, r.ID)
	}
}

func normalizeJobID(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "#")
	return s
}

func displayCommand(cmd string, max int) string {
	fields := strings.Fields(cmd)
	single := strings.Join(fields, " ")
	if len(single) <= max {
		return single
	}
	return single[:max-3] + "..."
}

func FormatElapsed(d time.Duration) string {
	total := int(d.Seconds())
	if total < 0 {
		total = 0
	}
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func truncateOutput(text string, maxLines, maxBytes int) (string, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines && len(text) <= maxBytes {
		return text, false
	}
	var out []string
	bytesCount := 0
	for i := len(lines) - 1; i >= 0 && len(out) < maxLines; i-- {
		sz := len(lines[i]) + 1
		if bytesCount+sz > maxBytes {
			break
		}
		out = append(out, lines[i])
		bytesCount += sz
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return "... (output truncated) ...\n" + strings.Join(out, "\n"), true
}

func intPtr(v int) *int {
	return &v
}
