package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/secrets"
	lore "github.com/setthasit/Lore/sdk"
)

const (
	clearScreen      = "\x1b[2J"
	clearScreenInert = `\x1b[2J`
)

var rawControls = []string{
	"\x00", "\a", "\b", "\r", "\x1b", "\x7f", "\x9b",
	"\u0085", "\u2028", "\u2029", "\u202e", "\u200f",
}

func assertInert(t *testing.T, out string) {
	t.Helper()

	for _, raw := range rawControls {
		if strings.Contains(out, raw) {
			t.Errorf("output carries the raw sequence %q\n--- output ---\n%s", raw, out)
		}
	}
	if !utf8.ValidString(out) {
		t.Errorf("output is not valid UTF-8\n--- output ---\n%s", out)
	}
}

func TestInertTextEscapesEverySequenceThatDrivesATerminal(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"an escape sequence", "seen" + clearScreen, "seen" + clearScreenInert},
		{"a bell", "ring\a", `ring\a`},
		{"a null byte", "a\x00b", `a\x00b`},
		{"a delete", "a\x7fb", `a\x7fb`},
		{"a backspace", "typo\bx", `typo\bx`},
		{"a carriage return rewriting the line", "seen\rhidden", `seen\rhidden`},
		{"a single-byte control sequence introducer", "a\u009b2Kb", `a\u009b2Kb`},
		{"a next line", "one\u0085two", `one\u0085two`},
		{"a line separator", "one\u2028two", `one\u2028two`},
		{"a paragraph separator", "one\u2029two", `one\u2029two`},
		{"an arabic letter mark", "a\u061cb", `a\u061cb`},
		{"a left-to-right mark", "a\u200eb", `a\u200eb`},
		{"a right-to-left mark", "a\u200fb", `a\u200fb`},
		{"the first bidi embedding control", "a\u202ab", `a\u202ab`},
		{"the last bidi embedding control", "a\u202eb", `a\u202eb`},
		{"the first bidi isolate control", "a\u2066b", `a\u2066b`},
		{"the last bidi isolate control", "a\u2069b", `a\u2069b`},
		{"a lone control sequence introducer byte", "a\x9b2Kb", `a\x9b2Kb`},
		{"a byte no encoding can hold", "a\xffb", `a\xffb`},
		{"a truncated multi-byte sequence", "a\xe2\x80", `a\xe2\x80`},
		{"a quote beside an escape", clearScreen + `"hi"`, clearScreenInert + `"hi"`},
		{"two controls in a row", "\x1b\x1b", `\x1b\x1b`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inertText(tt.text)
			if got != tt.want {
				t.Errorf("inertText(%q) = %q, want %q", tt.text, got, tt.want)
			}
			assertInert(t, got)
		})
	}
}

func TestInertTextLeavesLegitimateTextByteIdentical(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"a newline", "line one\nline two"},
		{"a tab", "col\tcol"},
		{"a zero-width non-joiner", "a\u200cb"},
		{"a zero-width joiner", "a\u200db, 👩\u200d💻"},
		{"a zero-width space", "a\u200bb"},
		{"a soft hyphen", "co\u00adoperate"},
		{"a byte order mark", "\ufeffworkspace"},
		{"a replacement character the document itself carries", "a\ufffdb"},
		{"non-latin scripts", "日本語 Проект مشروع"},
		{"an escape spelled out in text", `C:\x1b\path`},
		{"quoted text", `he said "no"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inertText(tt.text); got != tt.text {
				t.Errorf("inertText(%q) = %q, want the input back byte for byte", tt.text, got)
			}
		})
	}
}

func TestInertLineEscapesEveryBreakAndTabToo(t *testing.T) {
	const text = "one\ntwo\tthree\rfour\u0085five\u2028six\u2029seven"
	const want = `one\ntwo\tthree\rfour\u0085five\u2028six\u2029seven`

	got := inertLine(text)
	if got != want {
		t.Errorf("inertLine(%q) = %q, want %q", text, got, want)
	}
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("inertLine(%q) = %q, want it to stay on one line", text, got)
	}
}

func TestSinkScrubsTheInertRenderingOfARecordedSecret(t *testing.T) {
	var escaped []string
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if raw := string(r); inertLine(raw) != raw {
			escaped = append(escaped, raw)
		}
	}
	for b := 0x80; b <= 0xff; b++ {
		escaped = append(escaped, string([]byte{byte(b)}))
	}

	for _, raw := range escaped {
		secret := `fake"tok` + raw + "en-value"
		sink := &secrets.Sink{}
		sink.Record("token", secret)
		for render, rendered := range map[string]string{"inertLine": inertLine(secret), "inertText": inertText(secret)} {
			if got, want := sink.Scrub("title: "+rendered), "title: "+secrets.Placeholder; got != want {
				t.Errorf("Scrub(%s(%q)) = %q, want %q", render, secret, got, want)
			}
		}
	}
}

func TestInertTextReturnsCleanTextWithoutAllocating(t *testing.T) {
	const clean = "Storage design · notion page · café, 日本語, 👩\u200d💻"

	var got string
	allocs := testing.AllocsPerRun(100, func() { got = inertText(clean) })

	if allocs != 0 {
		t.Errorf("inertText allocates %.0f times for clean text, want the input returned as it is", allocs)
	}
	if got != clean {
		t.Errorf("inertText(%q) = %q, want the input back byte for byte", clean, got)
	}
}

func hostileBundle() *entities.EvidenceBundle {
	return &entities.EvidenceBundle{
		Question: "provenance of " + clearScreen + "Storage design",
		Anchor: entities.Anchor{
			Kind: entities.AnchorDocument | entities.AnchorTimeWindow,
			Doc: &entities.DocRef{
				ID:        anchorDoc.ID,
				Title:     "Storage\u202edesign",
				URL:       "https://notion.so/\adesign",
				CreatedAt: anchorDoc.CreatedAt,
			},
			Window: &entities.TimeWindow{
				From:       time.Date(2025, time.February, 10, 0, 0, 0, 0, time.UTC),
				To:         time.Date(2025, time.April, 11, 0, 0, 0, 0, time.UTC),
				Derivation: "event 'incident" + clearScreen + "X' via INC-201",
			},
		},
		Nodes: []entities.EvidenceNode{{
			Doc: entities.DocumentMeta{
				ID:        followUpDoc.ID,
				Source:    "github",
				Type:      lore.DocTypePR,
				Title:     "Index on" + clearScreen + " SQLite",
				Author:    "dev\u009b31m@example.test",
				URL:       "https://github.com/acme/lore/pull/\a12",
				CreatedAt: followUpDoc.CreatedAt,
			},
			Excerpt: "first line\x00\nsecond " + clearScreen + "line",
			Role:    entities.RoleFollowUp,
		}},
		Chains: [][]lore.DocID{{lore.DocID("notion:page:design" + clearScreen + "/storage"), followUpDoc.ID}},
		Gaps:   []string{"trail ends at PROJ" + clearScreen + "-4521"},
	}
}

func TestRenderBundleMakesEveryCitedFieldInert(t *testing.T) {
	var out bytes.Buffer
	renderBundle(&out, hostileBundle())

	got := out.String()
	for _, want := range []string{
		"provenance of " + clearScreenInert + "Storage design\n",
		`anchor: Storage\u202edesign` + "\n",
		`        https://notion.so/\adesign` + "\n",
		`window: 2025-02-10 .. 2025-04-11 (event 'incident` + clearScreenInert + `X' via INC-201)` + "\n",
		"2025-03-12 Index on" + clearScreenInert + " SQLite\n",
		`   github pr · dev\u009b31m@example.test · 2025-03-12 · follow_up` + "\n",
		`   https://github.com/acme/lore/pull/\a12` + "\n",
		`      first line\x00` + "\n" + "      second " + clearScreenInert + "line\n",
		"  notion:page:design" + clearScreenInert + "/storage → github:pr:12\n",
		"  trail ends at PROJ" + clearScreenInert + "-4521\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q\n--- output ---\n%s", want, got)
		}
	}
	assertInert(t, got)
}

func TestRenderBundleLaysOutACleanBundleExactly(t *testing.T) {
	var out bytes.Buffer
	renderBundle(&out, timelineBundle("provenance of Storage design"))

	const want = "provenance of Storage design\n" +
		"anchor: Storage design\n" +
		"        https://notion.so/design/storage\n" +
		"\n" +
		"2 documents\n" +
		"\n" +
		"2025-03-10 Storage design\n" +
		"   notion page · arch@example.test · 2025-03-10\n" +
		"   https://notion.so/design/storage\n" +
		"      postgres with pgvector was the alternative\n" +
		"\n" +
		"2025-03-12 Index on SQLite, not Postgres\n" +
		"   github pr · dev@example.test · 2025-03-12 · follow_up\n" +
		"   https://github.com/acme/lore/pull/12\n" +
		"      sqlite ships everywhere and needs no server\n" +
		"\n" +
		"chains:\n" +
		"  notion:page:design/storage → github:pr:12\n" +
		"\n" +
		"gaps:\n" +
		"  trail ends at PROJ-4521; no linked follow-up\n"

	if got := out.String(); got != want {
		t.Errorf("renderBundle() =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderBundleMakesABlamedSHAInert(t *testing.T) {
	bundle := timelineBundle("why " + whyFile)
	bundle.Anchor = entities.Anchor{
		Kind: entities.AnchorCodeSpan,
		Code: &entities.CodeAnchor{
			Repo:      "github:acme/lore",
			File:      whyFile,
			LineStart: 10,
			LineEnd:   13,
			BlamedSHAs: []string{
				"1111" + clearScreen + "111111111",
				"aaaaaaaaaaaé111111111111",
			},
		},
	}

	var out bytes.Buffer
	renderBundle(&out, bundle)

	got := out.String()
	want := `        blamed 1111` + clearScreenInert + `1111, aaaaaaaaaaa\xc3` + "\n"
	if !strings.Contains(got, want) {
		t.Errorf("output is missing %q\n--- output ---\n%s", want, got)
	}
	assertInert(t, got)
}

func TestRenderBundleMakesTheCodeAnchorRepoAndFileInert(t *testing.T) {
	const hostileRepo = "github:acme/" + clearScreen + "lore"
	const hostileFile = "internal/\aauth/\u202eog.og.go"
	const inertAnchor = "anchor: github:acme/" + clearScreenInert + `lore internal/\aauth/\u202eog.og.go`

	tests := []struct {
		name  string
		start int
		end   int
		want  string
	}{
		{name: "a line span", start: 10, end: 13, want: inertAnchor + ":10-13"},
		{name: "a whole file", want: inertAnchor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := timelineBundle("why " + whyFile)
			bundle.Anchor = entities.Anchor{
				Kind: entities.AnchorCodeSpan,
				Code: &entities.CodeAnchor{
					Repo:      hostileRepo,
					File:      hostileFile,
					LineStart: tt.start,
					LineEnd:   tt.end,
				},
			}

			var out bytes.Buffer
			renderBundle(&out, bundle)

			got := out.String()
			if !strings.Contains(got, tt.want+"\n") {
				t.Errorf("output is missing %q\n--- output ---\n%s", tt.want, got)
			}
			assertInert(t, got)
		})
	}
}
