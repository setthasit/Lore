package conform

import (
	"fmt"
	"slices"
	"testing"
)

type recordingReporter struct {
	lines        []string
	helpedAtLine int
	helped       bool
}

func (r *recordingReporter) Helper() {
	if !r.helped {
		r.helped = true
		r.helpedAtLine = len(r.lines)
	}
}

func (r *recordingReporter) Logf(format string, args ...any) {
	r.lines = append(r.lines, "log: "+fmt.Sprintf(format, args...))
}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.lines = append(r.lines, "error: "+fmt.Sprintf(format, args...))
}

func (r *recordingReporter) Fatal(args ...any) {
	r.lines = append(r.lines, "fatal: "+fmt.Sprint(args...))
}

func TestAStreamFailureReportsWhatItSkippedAndWhatFollowedItBeforeGivingUp(t *testing.T) {
	var reporter recordingReporter
	reportSkipsAndFindingsBeforeTheFatal(&reporter, Result{
		Findings: []Finding{
			{CheckStream, "stub: full stream: page 2 timed out"},
			{CheckStreamError, "full stream: the stream yielded another error after the first"},
		},
		Skipped: []Skip{
			{CheckCursors, "the stream broke after 1 batch(es), so what it delivered is not a complete sample"},
		},
	})

	want := []string{
		"log: every batch carries a cursor — the stream broke after 1 batch(es), so what it delivered is not a complete sample",
		"error: an error ends the stream: full stream: the stream yielded another error after the first",
		"fatal: stub: full stream: page 2 timed out",
	}
	if !slices.Equal(reporter.lines, want) {
		t.Errorf("reported\n%q\nwant\n%q", reporter.lines, want)
	}
	if !reporter.helped {
		t.Error("the reporting frame was not marked a helper, so every line lands on conform.go instead of the caller")
	}
	if reporter.helpedAtLine != 0 {
		t.Errorf("the reporting frame was marked a helper only after %d line(s)", reporter.helpedAtLine)
	}
}
