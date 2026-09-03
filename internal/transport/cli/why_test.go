package cli

import (
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/services"
)

const whyFile = "internal/auth/auth.go"

func TestParseLineSpan(t *testing.T) {
	tests := []struct {
		name string
		arg  string
		want lineSpan
		fail bool
	}{
		{
			name: "a span of lines",
			arg:  whyFile + ":10-20",
			want: lineSpan{file: whyFile, start: 10, end: 20},
		},
		{
			name: "a single line",
			arg:  whyFile + ":12",
			want: lineSpan{file: whyFile, start: 12},
		},
		{
			name: "a one-line span",
			arg:  whyFile + ":12-12",
			want: lineSpan{file: whyFile, start: 12, end: 12},
		},
		{
			name: "a path holding colons splits on the last one",
			arg:  "C:/dev/lore/" + whyFile + ":10-20",
			want: lineSpan{file: "C:/dev/lore/" + whyFile, start: 10, end: 20},
		},
		{
			name: "the service owns the lower bound",
			arg:  whyFile + ":0-20",
			want: lineSpan{file: whyFile, end: 20},
		},
		{name: "no span at all", arg: whyFile, fail: true},
		{name: "an empty span", arg: whyFile + ":", fail: true},
		{name: "no file", arg: ":10-20", fail: true},
		{name: "a non-numeric span", arg: whyFile + ":ten", fail: true},
		{name: "a path mistaken for a span", arg: "weird:path", fail: true},
		{name: "an unfinished span", arg: whyFile + ":10-", fail: true},
		{name: "a negative first line", arg: whyFile + ":-5-10", fail: true},
		{name: "a reversed span", arg: whyFile + ":40-12", fail: true},
		{name: "a three-part span", arg: whyFile + ":10-20-30", fail: true},
		{name: "an explicit zero end", arg: whyFile + ":10-0", fail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLineSpan(tt.arg)

			if tt.fail {
				if err == nil {
					t.Fatalf("parseLineSpan(%q) = %+v, want an error", tt.arg, got)
				}
				if !strings.Contains(err.Error(), tt.arg) {
					t.Errorf("error = %q, want it to name the argument %q", err, tt.arg)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLineSpan(%q): %v", tt.arg, err)
			}
			if got != tt.want {
				t.Errorf("parseLineSpan(%q) = %+v, want %+v", tt.arg, got, tt.want)
			}
		})
	}
}

func TestWhySendsTheParsedSpanToTheService(t *testing.T) {
	rt, why := mockWhy(t)
	why.EXPECT().
		Why(gomock.Any(), services.WhyRequest{
			Repo:      "github:acme/lore",
			File:      whyFile,
			LineStart: 10,
			LineEnd:   20,
			Question:  "why is the pool capped?",
		}).
		Return(timelineBundle("why "+whyFile), nil)

	res := run(t, rt, "why", whyFile+":10-20", "why is the pool capped?", "--repo=github:acme/lore")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "Storage design") {
		t.Errorf("stdout does not print the timeline:\n%s", res.stdout)
	}
}

func TestWhyPrintsTheAnchoredSpanAndItsBlamedCommits(t *testing.T) {
	bundle := timelineBundle("why " + whyFile)
	bundle.Anchor = entities.Anchor{
		Kind: entities.AnchorCodeSpan,
		Code: &entities.CodeAnchor{
			Repo:      "github:acme/lore",
			File:      whyFile,
			LineStart: 10,
			LineEnd:   13,
			BlamedSHAs: []string{
				"1111111111111111111111111111111111111111",
				"2222222222222222222222222222222222222222",
			},
		},
	}

	rt, why := mockWhy(t)
	why.EXPECT().Why(gomock.Any(), gomock.Any()).Return(bundle, nil)

	res := run(t, rt, "why", whyFile+":10-13")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	want := "anchor: github:acme/lore " + whyFile + ":10-13\n        blamed 111111111111, 222222222222\n"
	if !strings.Contains(res.stdout, want) {
		t.Errorf("output is missing %q\n--- output ---\n%s", want, res.stdout)
	}
}

func TestWhySendsASingleLineWithoutAnEnd(t *testing.T) {
	rt, why := mockWhy(t)
	why.EXPECT().
		Why(gomock.Any(), services.WhyRequest{File: whyFile, LineStart: 12}).
		Return(timelineBundle("why "+whyFile), nil)

	if res := run(t, rt, "why", whyFile+":12"); res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestWhyRejectsAMalformedSpanWithoutOpeningTheWorkspace(t *testing.T) {
	rt, _ := mockWhy(t)

	res := run(t, rt, "why", whyFile)

	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	if !strings.Contains(res.stderr, whyFile) {
		t.Errorf("stderr = %q, want it to name the argument", res.stderr)
	}
	if res.released {
		t.Error("the workspace was opened for an argument that never reaches the service")
	}
}

func TestWhyRefusesAnAskOnlyWorkspace(t *testing.T) {
	rt := &Runtime{Why: services.NewWhyService(nil, nil, services.QueryConfig{}, nil)}

	res := run(t, rt, "why", whyFile+":10-20")

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

func TestWhyExplainsTheTrailInProse(t *testing.T) {
	rt, why := mockWhy(t)
	synthesis := mockSynthesis(t, rt)
	bundle := timelineBundle("why " + whyFile)
	why.EXPECT().Why(gomock.Any(), gomock.Any()).Return(bundle, nil)
	synthesis.EXPECT().Synthesize(gomock.Any(), bundle.Question, bundle).Return(proseAnswer, nil)

	wantProse(t, run(t, rt, "why", whyFile+":10-20", "--explain"))
}

func TestWhyRawOutranksExplain(t *testing.T) {
	rt, why := mockWhy(t)
	mockSynthesis(t, rt)
	bundle := timelineBundle("why " + whyFile)
	why.EXPECT().Why(gomock.Any(), gomock.Any()).Return(bundle, nil)

	wantBundleJSON(t, run(t, rt, "why", whyFile+":10-20", "--raw", "--explain"), bundle)
}
