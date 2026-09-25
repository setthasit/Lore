package lore_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/wire"
)

var sdkPackage = reflect.TypeOf(lore.Document{}).PkgPath()

// Every type an out-of-process plugin exchanges with the host. A field added without a tag
// travels under its Go name, which is a silent protocol break.
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
		wire.Envelope{},
		wire.ManifestRequest{},
		wire.ShutdownRequest{},
		wire.ChangesRequest{},
		wire.EmbedRequest{},
		wire.CompleteRequest{},
		wire.BlameRequest{},
		wire.PathRequest{},
		wire.RemoteRequest{},
		wire.Frame{},
		wire.Batch{},
		wire.Error{},
	}
}

func TestEveryWireFieldCarriesASnakeCaseTag(t *testing.T) {
	for _, value := range wireTypes() {
		typ := reflect.TypeOf(value)
		t.Run(typ.String(), func(t *testing.T) {
			for i := range typ.NumField() {
				field := typ.Field(i)
				if !field.IsExported() || field.Anonymous {
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

func TestEveryListAndMapFieldEncodesEmptyRatherThanNull(t *testing.T) {
	for _, value := range wireTypes() {
		typ := reflect.TypeOf(value)
		t.Run(typ.String(), func(t *testing.T) {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshal a zero %s: %v", typ, err)
			}

			var encoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &encoded); err != nil {
				t.Fatalf("decode the encoded %s: %v", typ, err)
			}

			for i := range typ.NumField() {
				field := typ.Field(i)
				want := ""
				switch {
				case !field.IsExported():
					continue
				case isList(field.Type):
					want = "[]"
				case isMap(field.Type):
					want = "{}"
				default:
					continue
				}

				name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
				switch got, ok := encoded[name]; {
				case !ok:
					t.Errorf("zero %s omits %s; a collection field always travels", typ, name)
				case string(got) != want:
					t.Errorf("zero %s encodes %s as %s, want %s", typ, name, got, want)
				}
			}
		})
	}
}

// An unreferenced request type is invisible to reflection; only the declaration shows it.
func TestWireTypesEnumeratesEveryDeclaredWireStruct(t *testing.T) {
	listed := map[string]bool{}
	for _, value := range wireTypes() {
		listed[reflect.TypeOf(value).String()] = true
	}

	for _, dir := range []string{".", "wire"} {
		declared := taggedStructsIn(t, dir)
		if len(declared) == 0 {
			t.Errorf("%s holds no json-tagged struct declaration, so this guard scans nothing there", dir)
		}
		for _, name := range declared {
			if !listed[name] {
				t.Errorf("%s carries json tags but wireTypes() does not list it, so no test inspects it", name)
			}
		}
	}
}

func taggedStructsIn(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var out []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			declaration, ok := decl.(*ast.GenDecl)
			if !ok || declaration.Tok != token.TYPE {
				continue
			}
			for _, spec := range declaration.Specs {
				declared, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if structure, ok := declared.Type.(*ast.StructType); ok && carriesJSONTag(structure) {
					out = append(out, file.Name.Name+"."+declared.Name.Name)
				}
			}
		}
	}
	return out
}

func carriesJSONTag(structure *ast.StructType) bool {
	for _, field := range structure.Fields.List {
		if field.Tag != nil && strings.Contains(field.Tag.Value, "json:") {
			return true
		}
	}
	return false
}

// A struct named only by a field of another — wire.Error on a failed frame — is reached here
// whether it carries json tags or not.
func TestWireTypesEnumeratesEveryTypeItReaches(t *testing.T) {
	listed := map[reflect.Type]bool{}
	for _, value := range wireTypes() {
		listed[reflect.TypeOf(value)] = true
	}

	for _, value := range wireTypes() {
		for _, reached := range structsReachableFrom(reflect.TypeOf(value)) {
			if !listed[reached] {
				t.Errorf("%s carries %s, which wireTypes() does not list, so no test inspects it",
					reflect.TypeOf(value), reached)
			}
		}
	}
}

func structsReachableFrom(root reflect.Type) []reflect.Type {
	var out []reflect.Type
	seen := map[reflect.Type]bool{root: true}

	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] || !strings.HasPrefix(typ.PkgPath(), sdkPackage) {
			return
		}

		seen[typ] = true
		out = append(out, typ)
		for i := range typ.NumField() {
			walk(typ.Field(i).Type)
		}
	}
	for i := range root.NumField() {
		walk(root.Field(i).Type)
	}
	return out
}

// A json.RawMessage carries whole JSON rather than a list, and a byte slice encodes as a string.
func isList(t reflect.Type) bool {
	t = pointee(t)
	return t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8
}

func isMap(t reflect.Type) bool {
	return pointee(t).Kind() == reflect.Map
}

func pointee(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// The protocol requires RFC 3339 with an offset and both document timestamps present: a batch without them cannot be ordered.
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

	// The key is present even when the value is empty, so a plugin author sees every field the host expects.
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

// A batch is the checkpoint unit, so even one with no documents carries its cursor.
func TestEmptyBatchStillEncodesItsCursor(t *testing.T) {
	raw, err := json.Marshal(lore.Batch{Cursor: lore.Cursor{"updated_after": "2026-09-01T00:00:00Z"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"cursor":{"updated_after":"2026-09-01T00:00:00Z"}`) {
		t.Errorf("encoded batch %s does not carry its cursor", raw)
	}
	if !strings.Contains(string(raw), `"docs":[]`) {
		t.Errorf("encoded batch %s should declare docs as a list, as the protocol document does", raw)
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
		Secrets:      []lore.Secret{{Key: "api_key", DefaultEnv: "LORE_LINEAR_TOKEN"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, want := range []string{
		`"api_version":` + strconv.Itoa(lore.APIVersion),
		`"capabilities":{"embed":false,"complete":false,"repo_remotes":false}`,
		`"fields":[{"name":"teams","type":"string_list","required":true}]`,
		`"secrets":[{"key":"api_key","default_env":"LORE_LINEAR_TOKEN"}]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("encoded manifest %s is missing %s", raw, want)
		}
	}
}
