package lore_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
)

// Every type an out-of-process plugin exchanges with the host. A field added to
// one of these without a tag would travel under its Go name, which is a silent
// protocol break: the plugin would send `RepoRef` and the host would read
// `repo_ref` as empty. This test is the reason that cannot happen unnoticed.
func wireTypes() []any {
	return []any{
		lore.Document{},
		lore.RawRef{},
		lore.Batch{},
		lore.Manifest{},
		lore.Capabilities{},
		lore.Field{},
		lore.Secret{},
		lore.BlameSpan{},
		lore.CommitRef{},
	}
}

func TestEveryWireFieldCarriesASnakeCaseTag(t *testing.T) {
	for _, value := range wireTypes() {
		typ := reflect.TypeOf(value)
		t.Run(typ.Name(), func(t *testing.T) {
			for i := range typ.NumField() {
				field := typ.Field(i)
				if !field.IsExported() {
					continue
				}

				tag, ok := field.Tag.Lookup("json")
				if !ok {
					t.Errorf("%s.%s has no json tag, so it would travel under its Go name", typ.Name(), field.Name)
					continue
				}

				name, _, _ := strings.Cut(tag, ",")
				switch {
				case name == "":
					t.Errorf("%s.%s has an empty json name", typ.Name(), field.Name)
				case name != strings.ToLower(name):
					t.Errorf("%s.%s encodes as %q; the wire format is snake_case", typ.Name(), field.Name, name)
				case strings.Contains(name, "-"):
					t.Errorf("%s.%s encodes as %q; the wire format separates words with _", typ.Name(), field.Name, name)
				}
			}
		})
	}
}

// The protocol requires RFC 3339 with a timezone offset, and requires both
// document timestamps to be present: a batch that arrives without them cannot be
// ordered against anything already in the index.
func TestDocumentRoundTripsThroughTheWireFormat(t *testing.T) {
	created := time.Date(2026, time.August, 30, 14, 2, 11, 0, time.UTC)
	updated := time.Date(2026, time.September, 1, 7, 45, 3, 0, time.FixedZone("CEST", 2*60*60))

	want := lore.Document{
		ID:        lore.NewDocID("linear", lore.DocTypeTicket, "ENG-4471"),
		Source:    "linear",
		Type:      lore.DocTypeTicket,
		RepoRef:   "",
		Title:     "Move session store to Redis",
		Body:      "Chose B (Redis) over A (sticky sessions) because …",
		Author:    "grace@example.com",
		URL:       "https://linear.app/acme/issue/ENG-4471",
		CreatedAt: created,
		UpdatedAt: updated,
		Refs: []lore.RawRef{
			{Kind: lore.RefKindURL, Value: "https://github.com/acme/api/pull/812"},
			{Kind: lore.RefKindTicketKey, Value: "PROJ-123"},
		},
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The key is always present even when the value is empty, so a plugin author
	// reading the wire form sees every field the host expects.
	for _, key := range []string{
		`"id":`, `"source":`, `"type":`, `"repo_ref":`, `"title":`, `"body":`,
		`"author":`, `"url":`, `"created_at":`, `"updated_at":`, `"refs":`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("encoded document %s is missing %s", raw, key)
		}
	}
	if !strings.Contains(string(raw), `"updated_at":"2026-09-01T07:45:03+02:00"`) {
		t.Errorf("encoded document %s does not carry the offset the protocol requires", raw)
	}

	var got lore.Document
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("timestamps = %s / %s, want %s / %s", got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}

	got.CreatedAt, got.UpdatedAt = want.CreatedAt, want.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// A batch whose documents are empty still carries its cursor: the batch is the
// checkpoint unit, so a cursor-less frame checkpoints nothing.
func TestEmptyBatchStillEncodesItsCursor(t *testing.T) {
	raw, err := json.Marshal(lore.Batch{Cursor: lore.Cursor{"updated_after": "2026-09-01T00:00:00Z"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"cursor":{"updated_after":"2026-09-01T00:00:00Z"}`) {
		t.Errorf("encoded batch %s does not carry its cursor", raw)
	}
	if !strings.Contains(string(raw), `"docs":null`) {
		t.Errorf("encoded batch %s should still declare docs", raw)
	}
}

func TestManifestEncodesTheHandshakeShape(t *testing.T) {
	raw, err := json.Marshal(lore.Manifest{
		Name:         "linear",
		Kind:         lore.KindSource,
		APIVersion:   lore.APIVersion,
		Summary:      "Linear issues and comments; created_at is the issue createdAt",
		Capabilities: lore.Capabilities{},
		Fields:       []lore.Field{{Name: "teams", Type: lore.FieldStringList, Required: true}},
		Secrets:      []lore.Secret{{Key: "api_key", ConfigField: "token_env", DefaultEnv: "LORE_LINEAR_TOKEN"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, want := range []string{
		`"api_version":1`,
		`"capabilities":{"embed":false,"complete":false,"repo_remotes":false}`,
		`"fields":[{"name":"teams","type":"string_list","required":true}]`,
		`"secrets":[{"key":"api_key","config_field":"token_env","default_env":"LORE_LINEAR_TOKEN"}]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("encoded manifest %s is missing %s", raw, want)
		}
	}
}
