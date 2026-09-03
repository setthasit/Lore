package refscan

import (
	"slices"
	"testing"

	"github.com/setthasit/Lore/internal/entities"
)

func TestSetKeepsFirstSeenOrderAndDropsRepeats(t *testing.T) {
	var s Set
	s.Add(entities.RefKindPRNumber, "acme/widgets#42")
	s.AddAll(entities.RefKindCommitSHA, []string{"1a2b3c4", "9f8e7d6", "1a2b3c4"})
	s.Add(entities.RefKindPRNumber, "acme/widgets#42")
	s.Add(entities.RefKindTicketKey, "")
	s.Add(entities.RefKindTicketKey, "PROJ-123")

	want := []entities.RawRef{
		{Kind: entities.RefKindPRNumber, Value: "acme/widgets#42"},
		{Kind: entities.RefKindCommitSHA, Value: "1a2b3c4"},
		{Kind: entities.RefKindCommitSHA, Value: "9f8e7d6"},
		{Kind: entities.RefKindTicketKey, Value: "PROJ-123"},
	}
	if got := s.Refs(); !slices.Equal(got, want) {
		t.Errorf("Refs()\n got %v\nwant %v", got, want)
	}
}

func TestZeroSetHasNoRefs(t *testing.T) {
	var s Set
	if got := s.Refs(); len(got) != 0 {
		t.Errorf("Refs() = %v, want none", got)
	}
}

func TestKindIsPartOfIdentity(t *testing.T) {
	var s Set
	s.Add(entities.RefKindTicketKey, "PROJ-123")
	s.Add(entities.RefKindURL, "PROJ-123")

	if got := len(s.Refs()); got != 2 {
		t.Errorf("len(Refs()) = %d, want 2", got)
	}
}

func TestTextScanners(t *testing.T) {
	tests := []struct {
		name string
		scan func(*Set, string)
		text string
		want []entities.RawRef
	}{
		{
			name: "ticket keys: repeats collapse, lowercase is not a key",
			scan: (*Set).AddTicketKeys,
			text: "PROJ-123 blocks PROJ-456 which reverts PROJ-123 (not proj-789)",
			want: []entities.RawRef{
				{Kind: entities.RefKindTicketKey, Value: "PROJ-123"},
				{Kind: entities.RefKindTicketKey, Value: "PROJ-456"},
			},
		},
		{
			name: "ticket keys: prose without a key",
			scan: (*Set).AddTicketKeys,
			text: "no keys in this sentence",
			want: nil,
		},
		{
			name: "urls: sentence period trimmed, markdown link yields a bare url",
			scan: (*Set).AddURLs,
			text: "See https://www.notion.so/acme/Auth-spec-abc123. Also [spec](https://example.com/spec).",
			want: []entities.RawRef{
				{Kind: entities.RefKindURL, Value: "https://www.notion.so/acme/Auth-spec-abc123"},
				{Kind: entities.RefKindURL, Value: "https://example.com/spec"},
			},
		},
		{
			name: "urls: a bare host is not a url",
			scan: (*Set).AddURLs,
			text: "ping example.com when it breaks",
			want: nil,
		},
		{
			name: "commit shas: abbreviated and full, too short is skipped",
			scan: (*Set).AddCommitSHAs,
			text: "reverts 1a2b3c4 and 0123456789abcdef0123456789abcdef01234567, not 1a2b3c",
			want: []entities.RawRef{
				{Kind: entities.RefKindCommitSHA, Value: "1a2b3c4"},
				{Kind: entities.RefKindCommitSHA, Value: "0123456789abcdef0123456789abcdef01234567"},
			},
		},
		{
			name: "commit shas: uppercase hex is not a sha",
			scan: (*Set).AddCommitSHAs,
			text: "see 1A2B3C4 for the revert",
			want: nil,
		},
		{
			name: "file paths: nested paths, backticks and a sentence period",
			scan: (*Set).AddFilePaths,
			text: "`cmd/lore/main.go` calls internal/auth/auth.go, described in docs/v3/04-connectors-and-sync.md.",
			want: []entities.RawRef{
				{Kind: entities.RefKindFilePath, Value: "cmd/lore/main.go"},
				{Kind: entities.RefKindFilePath, Value: "internal/auth/auth.go"},
				{Kind: entities.RefKindFilePath, Value: "docs/v3/04-connectors-and-sync.md"},
			},
		},
		{
			name: "file paths: bare filename without a slash",
			scan: (*Set).AddFilePaths,
			text: "update README.md before release",
			want: nil,
		},
		{
			name: "file paths: repo slug without an extension",
			scan: (*Set).AddFilePaths,
			text: "moved to acme/widgets last quarter",
			want: nil,
		},
		{
			name: "file paths: issue cross-reference",
			scan: (*Set).AddFilePaths,
			text: "tracked in acme/widgets#41",
			want: nil,
		},
		{
			name: "file paths: version-like token",
			scan: (*Set).AddFilePaths,
			text: "bumped 1.5/2.0 compatibility",
			want: nil,
		},
		{
			name: "file paths: a url's own path is not a workspace path",
			scan: (*Set).AddFilePaths,
			text: "see https://github.com/acme/widgets/blob/main/internal/auth/auth.go for the change",
			want: nil,
		},
		{
			name: "file paths: a real path next to a url still matches",
			scan: (*Set).AddFilePaths,
			text: "https://example.com/spec.html covers internal/auth/auth.go",
			want: []entities.RawRef{
				{Kind: entities.RefKindFilePath, Value: "internal/auth/auth.go"},
			},
		},
		{
			name: "file paths: bare word",
			scan: (*Set).AddFilePaths,
			text: "connectors",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s Set
			tt.scan(&s, tt.text)
			if got := s.Refs(); !slices.Equal(got, tt.want) {
				t.Errorf("refs of %q\n got %v\nwant %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestTextScannersIgnoreEmptyText(t *testing.T) {
	scanners := []struct {
		name string
		scan func(*Set, string)
	}{
		{"AddTicketKeys", (*Set).AddTicketKeys},
		{"AddURLs", (*Set).AddURLs},
		{"AddCommitSHAs", (*Set).AddCommitSHAs},
		{"AddFilePaths", (*Set).AddFilePaths},
	}

	for _, sc := range scanners {
		t.Run(sc.name, func(t *testing.T) {
			var s Set
			sc.scan(&s, "")
			if got := s.Refs(); len(got) != 0 {
				t.Errorf("%s on empty text added %v", sc.name, got)
			}
		})
	}
}
