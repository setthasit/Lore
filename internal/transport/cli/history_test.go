package cli

import (
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/services"
)

const (
	historyPath = "internal/auth/auth.go"
	historyRepo = "github:acme/lore"
)

func fileHistoryBundle() *entities.EvidenceBundle {
	bundle := timelineBundle("history of " + historyPath + " in " + historyRepo)
	bundle.Anchor = entities.Anchor{
		Kind: entities.AnchorCodeSpan,
		Code: &entities.CodeAnchor{
			Repo: historyRepo,
			File: historyPath,
			BlamedSHAs: []string{
				"1111111111111111111111111111111111111111",
				"2222222222222222222222222222222222222222",
			},
		},
	}

	return bundle
}

func TestHistorySendsThePagingFlagsToTheService(t *testing.T) {
	rt, history := mockHistory(t)
	history.EXPECT().
		HistoryOf(gomock.Any(), services.HistoryRequest{
			Repo:   historyRepo,
			File:   historyPath,
			Limit:  5,
			Before: "9f8e7d6",
		}).
		Return(fileHistoryBundle(), nil)

	res := run(t, rt, "history", historyPath, "--repo="+historyRepo, "--limit=5", "--before=9f8e7d6")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{"2025-03-10 Storage design", "2025-03-12 Index on SQLite, not Postgres"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output is missing %q\n--- output ---\n%s", want, res.stdout)
		}
	}
}

func TestHistoryDefaultsThePageToTheService(t *testing.T) {
	rt, history := mockHistory(t)
	history.EXPECT().
		HistoryOf(gomock.Any(), services.HistoryRequest{File: historyPath}).
		Return(fileHistoryBundle(), nil)

	if res := run(t, rt, "history", historyPath); res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestHistoryPrintsAWholeFileAnchorWithoutALineSpan(t *testing.T) {
	rt, history := mockHistory(t)
	history.EXPECT().HistoryOf(gomock.Any(), gomock.Any()).Return(fileHistoryBundle(), nil)

	res := run(t, rt, "history", historyPath)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	want := "anchor: " + historyRepo + " " + historyPath + "\n        blamed 111111111111, 222222222222\n"
	if !strings.Contains(res.stdout, want) {
		t.Errorf("output is missing %q\n--- output ---\n%s", want, res.stdout)
	}
	if strings.Contains(res.stdout, historyPath+":") {
		t.Errorf("the whole-file anchor prints a line span:\n%s", res.stdout)
	}
}

func TestHistoryRawEmitsTheCanonicalBundleJSON(t *testing.T) {
	rt, history := mockHistory(t)
	bundle := fileHistoryBundle()
	history.EXPECT().HistoryOf(gomock.Any(), gomock.Any()).Return(bundle, nil)

	res := run(t, rt, "history", historyPath, "--raw")
	wantBundleJSON(t, res, bundle)

	for _, field := range []string{"line_start", "line_end"} {
		if strings.Contains(res.stdout, field) {
			t.Errorf("the whole-file anchor encodes %q:\n%s", field, res.stdout)
		}
	}
}

func TestHistoryExplainsTheTimelineInProse(t *testing.T) {
	rt, history := mockHistory(t)
	synthesis := mockSynthesis(t, rt)
	bundle := fileHistoryBundle()
	history.EXPECT().HistoryOf(gomock.Any(), gomock.Any()).Return(bundle, nil)
	synthesis.EXPECT().Synthesize(gomock.Any(), bundle.Question, bundle).Return(proseAnswer, nil)

	wantProse(t, run(t, rt, "history", historyPath, "--explain"))
}

func TestHistoryRawOutranksExplain(t *testing.T) {
	rt, history := mockHistory(t)
	mockSynthesis(t, rt)
	bundle := fileHistoryBundle()
	history.EXPECT().HistoryOf(gomock.Any(), gomock.Any()).Return(bundle, nil)

	wantBundleJSON(t, run(t, rt, "history", historyPath, "--raw", "--explain"), bundle)
}

func TestHistoryWithoutAPathIsAUsageError(t *testing.T) {
	rt, _ := mockHistory(t)

	res := run(t, rt, "history")
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	if res.released {
		t.Error("the workspace was built for an invocation that could not run")
	}
}

func TestHistoryRefusesAnAskOnlyWorkspace(t *testing.T) {
	rt := &Runtime{History: services.NewHistoryService(nil, nil)}

	res := run(t, rt, "history", historyPath)

	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitPrecondition, res.stderr)
	}
	want := "no repositories registered — code anchoring disabled for this workspace"
	if !strings.Contains(res.stderr, want) {
		t.Errorf("stderr = %q, want it to carry %q", res.stderr, want)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want nothing printed", res.stdout)
	}
	if !res.released {
		t.Error("the workspace was not released")
	}
}

func TestHistoryHelpDocumentsThePagingCursor(t *testing.T) {
	res := run(t, nil, "history", "--help")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	for _, want := range []string{"--before", "blamed", "older", "exhausted", "caps it"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("help does not explain %q\n--- help ---\n%s", want, res.stdout)
		}
	}
}
