package progress

import (
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedWriter lets the standard logger write from the test and the reporter
// goroutine concurrently.
type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// captureLogs redirects the standard logger into a locked builder for the
// test's lifetime and returns the builder with its lock.
func captureLogs(t *testing.T) (*strings.Builder, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var out strings.Builder
	log.SetOutput(&lockedWriter{mu: &mu, w: &out})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &out, &mu
}

// trickle yields one byte per read with a pause between reads, so a transfer
// of a few kilobytes spans several shrunk intervals.
type trickle struct{ pause time.Duration }

func (t trickle) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	time.Sleep(t.pause)
	p[0] = 'x'
	return 1, nil
}

// slowTransfer copies n bytes through a counting Reader at trickle's pace,
// as io.Copy would.
func slowTransfer(t *testing.T, r *Reader, n int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.CopyN(io.Discard, r, int64(n))
	}()
	t.Cleanup(func() { <-done })
}

// TestReportLogsPositionRateAndTotal pins the in-flight line: while a slow
// transfer runs, the reporter names the transfer, its position against the
// total, the percentage and the rate of the last interval.
func TestReportLogsPositionRateAndTotal(t *testing.T) {
	oldInterval := Interval
	Interval = 5 * time.Millisecond
	t.Cleanup(func() { Interval = oldInterval })

	out, mu := captureLogs(t)

	body := NewReader(trickle{pause: time.Millisecond})
	stop := Report("test", "restoring", "blob.bin", body.Count, 4000)
	slowTransfer(t, body, 2000)
	time.Sleep(30 * time.Millisecond)
	stop()

	mu.Lock()
	defer mu.Unlock()
	logs := out.String()
	if !strings.Contains(logs, "test: restoring blob.bin") {
		t.Fatalf("no in-flight line naming the transfer in logs:\n%s", logs)
	}
	// position / total (percentage) at rate — the numbers themselves are
	// timing-dependent, so the assertion pins the line's shape only.
	if !strings.Contains(logs, " / ") || !strings.Contains(logs, "%) at ") {
		t.Fatalf("no position/total/percentage/rate line in logs:\n%s", logs)
	}
}

// TestReportWithoutTotalDropsThePercentage covers the unknown-size shape: the
// line carries position and rate but no total and no percentage.
func TestReportWithoutTotalDropsThePercentage(t *testing.T) {
	oldInterval := Interval
	Interval = 5 * time.Millisecond
	t.Cleanup(func() { Interval = oldInterval })

	out, mu := captureLogs(t)

	body := NewReader(trickle{pause: time.Millisecond})
	stop := Report("test", "fetching", "blob.bin", body.Count, -1)
	slowTransfer(t, body, 2000)
	time.Sleep(30 * time.Millisecond)
	stop()

	mu.Lock()
	defer mu.Unlock()
	logs := out.String()
	if !strings.Contains(logs, "test: fetching blob.bin") || !strings.Contains(logs, " at ") {
		t.Fatalf("no position/rate line in logs:\n%s", logs)
	}
	if strings.Contains(logs, "%") {
		t.Fatalf("unknown total still logged a percentage:\n%s", logs)
	}
}

// TestStoppableReporterEmitsNoLaterLines pins the stop semantics: once the
// caller stops the reporter, no further lines may appear, so a finished
// transfer cannot keep logging into the next one's window.
func TestStoppableReporterEmitsNoLaterLines(t *testing.T) {
	oldInterval := Interval
	Interval = 5 * time.Millisecond
	t.Cleanup(func() { Interval = oldInterval })

	out, mu := captureLogs(t)

	body := NewReader(trickle{pause: time.Millisecond})
	stop := Report("test", "restoring", "blob.bin", body.Count, 4000)
	slowTransfer(t, body, 2000)
	time.Sleep(30 * time.Millisecond)
	stop()

	mu.Lock()
	lines := strings.Count(out.String(), "\n")
	mu.Unlock()

	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Count(out.String(), "\n"); got != lines {
		t.Fatalf("reporter logged %d more line(s) after stop (%d -> %d)", got-lines, lines, got)
	}
}

// TestFormatSize pins the rendering progress lines use: plain bytes below one
// kibibyte, then binary units with one decimal.
func TestFormatSize(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{512 * 1024, "512.0 KiB"},
		{1.5 * 1024 * 1024 * 1024, "1.5 GiB"},
	}
	for _, tc := range cases {
		if got := FormatSize(tc.in); got != tc.want {
			t.Errorf("FormatSize(%v)=%q want %q", tc.in, got, tc.want)
		}
	}
}
