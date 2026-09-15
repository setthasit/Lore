package wire_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/wire"
)

func assertEncodesTo(t *testing.T, value any, want string) {
	t.Helper()

	got, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("encoded as\n%s\nwant\n%s", got, want)
	}
}

func TestRequestsEncodeTheProtocolFieldNames(t *testing.T) {
	envelope := func(id, op string) wire.Envelope {
		return wire.Envelope{V: lore.APIVersion, ID: id, Op: op}
	}
	secrets := map[string]string{"token": "value"}

	cases := []struct {
		name    string
		request any
		want    string
	}{
		{
			name:    "manifest",
			request: wire.ManifestRequest{Envelope: envelope("p-1", wire.OpManifest)},
			want:    `{"v":1,"id":"p-1","op":"manifest"}`,
		},
		{
			name:    "shutdown",
			request: wire.ShutdownRequest{Envelope: envelope("p-2", wire.OpShutdown)},
			want:    `{"v":1,"id":"p-2","op":"shutdown"}`,
		},
		{
			name: "changes",
			request: wire.ChangesRequest{
				Envelope: envelope("p-3", wire.OpChanges),
				Instance: "github-main",
				Config:   json.RawMessage(`{"repo":"owner/name"}`),
				Secrets:  secrets,
				Cursor:   lore.Cursor{"since": "42"},
			},
			want: `{"v":1,"id":"p-3","op":"changes","instance":"github-main","config":{"repo":"owner/name"},"secrets":{"token":"value"},"cursor":{"since":"42"}}`,
		},
		{
			name: "embed",
			request: wire.EmbedRequest{
				Envelope: envelope("p-4", wire.OpEmbed),
				Config:   json.RawMessage(`{}`),
				Secrets:  map[string]string{},
				Model:    "text-embed-3",
				Texts:    []string{"first", "second"},
			},
			want: `{"v":1,"id":"p-4","op":"embed","config":{},"secrets":{},"model":"text-embed-3","texts":["first","second"]}`,
		},
		{
			name: "complete",
			request: wire.CompleteRequest{
				Envelope: envelope("p-5", wire.OpComplete),
				Config:   json.RawMessage(`{}`),
				Secrets:  secrets,
				Model:    "gpt-mini",
				System:   "be terse",
				User:     "why",
			},
			want: `{"v":1,"id":"p-5","op":"complete","config":{},"secrets":{"token":"value"},"model":"gpt-mini","system":"be terse","user":"why"}`,
		},
		{
			name: "blame",
			request: wire.BlameRequest{
				Envelope:  envelope("p-6", wire.OpBlame),
				Path:      "internal/app/app.go",
				StartLine: 3,
				EndLine:   9,
			},
			want: `{"v":1,"id":"p-6","op":"blame","path":"internal/app/app.go","start_line":3,"end_line":9}`,
		},
		{
			name:    "log",
			request: wire.PathRequest{Envelope: envelope("p-7", wire.OpLog), Path: "README.md"},
			want:    `{"v":1,"id":"p-7","op":"log","path":"README.md"}`,
		},
		{
			name:    "has_file",
			request: wire.PathRequest{Envelope: envelope("p-8", wire.OpHasFile), Path: "README.md"},
			want:    `{"v":1,"id":"p-8","op":"has_file","path":"README.md"}`,
		},
		{
			name: "matches_remote",
			request: wire.RemoteRequest{
				Envelope: envelope("p-9", wire.OpRemote),
				Instance: "github-main",
				Config:   json.RawMessage(`{}`),
				Secrets:  map[string]string{},
				Remote:   "github:owner/name",
			},
			want: `{"v":1,"id":"p-9","op":"matches_remote","instance":"github-main","config":{},"secrets":{},"remote":"github:owner/name"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertEncodesTo(t, c.request, c.want)
		})
	}
}

func TestFrameEncodesEveryAnswerFieldOnEveryFrame(t *testing.T) {
	created := time.Date(2026, time.August, 30, 14, 2, 11, 0, time.UTC)
	updated := time.Date(2026, time.September, 1, 7, 45, 3, 0, time.FixedZone("CEST", 2*60*60))
	cursor := lore.Cursor{"since": "42"}

	cases := []struct {
		name  string
		frame wire.Frame
		want  string
	}{
		{
			name: "manifest",
			frame: wire.Frame{
				V:  lore.APIVersion,
				ID: "p-1",
				OK: true,
				Manifest: &lore.Manifest{
					Name:         "github",
					Kind:         lore.KindSource,
					APIVersion:   lore.APIVersion,
					Summary:      "issues and pull requests",
					Capabilities: lore.Capabilities{RepoRemotes: true},
				},
			},
			want: `{"v":1,"id":"p-1","ok":true,"done":false,"error":null,` +
				`"manifest":{"name":"github","kind":"source","api_version":1,"summary":"issues and pull requests",` +
				`"capabilities":{"embed":false,"complete":false,"repo_remotes":true},"fields":[],"secrets":[],"default_models":{}},` +
				`"batch":null,"vectors":[],"dimensions":0,"text":"","spans":[],"commits":[],"present":false,"matches":false}`,
		},
		{
			name: "changes batch",
			frame: wire.Frame{
				V:  lore.APIVersion,
				ID: "p-3",
				OK: true,
				Batch: &wire.Batch{
					Docs: []lore.Document{{
						ID:        lore.NewDocID("github-main", lore.DocTypeIssue, "7"),
						Source:    "github-main",
						Type:      lore.DocTypeIssue,
						RepoRef:   "github:owner/name",
						Title:     "flaky test",
						Body:      "it fails",
						Author:    "ann",
						URL:       "https://example.test/7",
						CreatedAt: created,
						UpdatedAt: updated,
						Refs:      []lore.RawRef{{Kind: lore.RefKindCommitSHA, Value: "deadbeef"}},
					}},
					Cursor: &cursor,
				},
			},
			want: `{"v":1,"id":"p-3","ok":true,"done":false,"error":null,"manifest":null,` +
				`"batch":{"docs":[{"id":"github-main:issue:7","source":"github-main","type":"issue","repo_ref":"github:owner/name",` +
				`"title":"flaky test","body":"it fails","author":"ann","url":"https://example.test/7",` +
				`"created_at":"2026-08-30T14:02:11Z","updated_at":"2026-09-01T07:45:03+02:00",` +
				`"refs":[{"kind":"commit_sha","value":"deadbeef"}]}],"cursor":{"since":"42"}},` +
				`"vectors":[],"dimensions":0,"text":"","spans":[],"commits":[],"present":false,"matches":false}`,
		},
		{
			name: "embed vectors",
			frame: wire.Frame{
				V:          lore.APIVersion,
				ID:         "p-4",
				OK:         true,
				Done:       true,
				Vectors:    [][]float32{{0.5, -1.25}},
				Dimensions: 2,
			},
			want: `{"v":1,"id":"p-4","ok":true,"done":true,"error":null,"manifest":null,"batch":null,` +
				`"vectors":[[0.5,-1.25]],"dimensions":2,"text":"","spans":[],"commits":[],"present":false,"matches":false}`,
		},
		{
			name: "embed vectors with an empty row",
			frame: wire.Frame{
				V:          lore.APIVersion,
				ID:         "p-4",
				OK:         true,
				Done:       true,
				Vectors:    [][]float32{nil},
				Dimensions: 2,
			},
			want: `{"v":1,"id":"p-4","ok":true,"done":true,"error":null,"manifest":null,"batch":null,` +
				`"vectors":[[]],"dimensions":2,"text":"","spans":[],"commits":[],"present":false,"matches":false}`,
		},
		{
			name: "blame spans and log commits",
			frame: wire.Frame{
				V:    lore.APIVersion,
				ID:   "p-6",
				OK:   true,
				Done: true,
				Spans: []lore.BlameSpan{{
					SHA:       "deadbeef",
					LineStart: 3,
					LineEnd:   4,
					Author:    "ann",
					Time:      created,
					Lines:     []string{"package app", ""},
				}},
				Commits: []lore.CommitRef{{SHA: "deadbeef", Author: "ann", Time: created, Subject: "init"}},
				Present: true,
				Matches: true,
			},
			want: `{"v":1,"id":"p-6","ok":true,"done":true,"error":null,"manifest":null,"batch":null,` +
				`"vectors":[],"dimensions":0,"text":"",` +
				`"spans":[{"sha":"deadbeef","line_start":3,"line_end":4,"author":"ann","time":"2026-08-30T14:02:11Z","lines":["package app",""]}],` +
				`"commits":[{"sha":"deadbeef","author":"ann","time":"2026-08-30T14:02:11Z","subject":"init"}],` +
				`"present":true,"matches":true}`,
		},
		{
			name: "failure",
			frame: wire.Frame{
				V:     lore.APIVersion,
				ID:    "p-3",
				Done:  true,
				Error: &wire.Error{Message: "rate limited", Kind: "transient"},
			},
			want: `{"v":1,"id":"p-3","ok":false,"done":true,"error":{"message":"rate limited","kind":"transient"},` +
				`"manifest":null,"batch":null,"vectors":[],"dimensions":0,"text":"","spans":[],"commits":[],` +
				`"present":false,"matches":false}`,
		},
		{
			name:  "completion text",
			frame: wire.Frame{V: lore.APIVersion, ID: "p-5", OK: true, Done: true, Text: "an answer"},
			want: `{"v":1,"id":"p-5","ok":true,"done":true,"error":null,"manifest":null,"batch":null,` +
				`"vectors":[],"dimensions":0,"text":"an answer","spans":[],"commits":[],"present":false,"matches":false}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertEncodesTo(t, c.frame, c.want)
		})
	}
}
