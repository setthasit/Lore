package jira

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFlatten(t *testing.T) {
	tests := []struct {
		fixture string
		want    string
	}{
		{
			fixture: "adf_paragraph.json",
			want:    "See [the ADR](https://example.invalid/adr-7) first.\nThen ship.",
		},
		{
			fixture: "adf_heading.json",
			want:    "# Decision\n### Context",
		},
		{
			fixture: "adf_lists.json",
			want:    "- Outer\n  - Inner\n- Second\n- First step",
		},
		{
			fixture: "adf_codeblock.json",
			want:    "```go\nif err != nil {\n\treturn err\n}\n```",
		},
		{
			fixture: "adf_blockquote.json",
			want:    "> We tried this in 2023.\n> It regressed.",
		},
		{
			fixture: "adf_panel.json",
			want:    "Do not retry blindly.",
		},
		{
			fixture: "adf_rule.json",
			want:    "Above\n---\nBelow",
		},
		{
			fixture: "adf_table.json",
			want:    "Option | Cost\nDrop | Data loss",
		},
		{
			fixture: "adf_inline.json",
			want:    "https://example.invalid/card @Ada Lovelace :thumbsup: IN PROGRESS due 2024-05-03",
		},
		{
			fixture: "adf_unknown.json",
			want:    "diagram.png\nAfter the media.",
		},
		{
			fixture: "adf_deep.json",
			want:    "shallow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			if got := flatten(loadADF(t, tt.fixture)); got != tt.want {
				t.Errorf("flatten\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestFlattenAbsentDocument(t *testing.T) {
	if got := flatten(nil); got != "" {
		t.Errorf("flatten(nil) = %q, want empty", got)
	}
}

func TestHeadingPrefixClampsLevel(t *testing.T) {
	tests := []struct {
		level int
		want  string
	}{
		{level: 0, want: "#"},
		{level: 1, want: "#"},
		{level: 6, want: "######"},
		{level: 9, want: "######"},
	}
	for _, tt := range tests {
		if got := headingPrefix(tt.level); got != tt.want {
			t.Errorf("headingPrefix(%d) = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestEpochDateFallsBackToTheRawValue(t *testing.T) {
	if got := epochDate("1714694400000"); got != "2024-05-03" {
		t.Errorf("epochDate(millis) = %q, want %q", got, "2024-05-03")
	}
	if got := epochDate("next friday"); got != "next friday" {
		t.Errorf("epochDate(unparseable) = %q, want it echoed", got)
	}
}

func loadADF(t *testing.T, name string) *adfNode {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var doc adfNode
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return &doc
}
