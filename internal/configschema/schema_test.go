package configschema

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

func TestDurationPatternAcceptsExactlyWhatTheLoaderParses(t *testing.T) {
	pattern := regexp.MustCompile(durationPattern)
	for _, raw := range []string{
		"30m", "1h30m", "1.5h", ".5s", "5.h", "250ms", "3µs", "3μs", "0", "-0", "-2h", "+3d", "30d",
		"", "30", "00", "d", ".h", "1.5d", "1d2h", "30 d", " 30m", "5x", "-", "+",
	} {
		_, err := lore.ParseDuration(raw)
		if got, want := pattern.MatchString(raw), err == nil; got != want {
			t.Errorf("pattern matches %q = %t, but the loader parses it = %t", raw, got, want)
		}
	}
}

func TestSchemaOffersExactlyTheKeysTheLoaderDecodes(t *testing.T) {
	assertCoversStruct(t, "lore.yaml", reflect.TypeFor[config.Config](), generate(t, Catalog{}))
}

func assertCoversStruct(t *testing.T, at string, typ reflect.Type, schema map[string]any) {
	t.Helper()

	properties, _ := schema["properties"].(map[string]any)
	decoded := make(map[string]bool)
	for i := range typ.NumField() {
		f := typ.Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if key == "" || key == "-" {
			continue
		}
		decoded[key] = true

		property, ok := properties[key].(map[string]any)
		if !ok {
			t.Errorf("%s.%s is decoded by the loader, but the schema flags it as unknown", at, key)
			continue
		}
		if nested := configStruct(f.Type); nested != nil {
			if items, isList := property["items"].(map[string]any); isList {
				property = items
			}
			assertCoversStruct(t, at+"."+key, nested, property)
		}
	}
	for key := range properties {
		if !decoded[key] {
			t.Errorf("%s.%s is offered by the schema, but the loader rejects it", at, key)
		}
	}
}

func configStruct(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Struct && typ.PkgPath() == reflect.TypeFor[config.Config]().PkgPath() {
		return typ
	}
	return nil
}

func TestAWithBlockRequiresASecretOnlyWhenNothingFallsBack(t *testing.T) {
	manifest := lore.Manifest{
		Name:   "tracker",
		Kind:   lore.KindSource,
		Fields: []lore.Field{{Name: "team", Type: lore.FieldString, Required: true}},
		Secrets: []lore.Secret{
			{Key: "token", DefaultEnv: "TRACKER_TOKEN"},
			{Key: "webhook_secret", Optional: true},
		},
	}
	for _, test := range []struct {
		name   string
		origin string
		want   []string
	}{
		{name: "compiled in, so token falls back to its default variable", origin: registry.OriginBuiltin, want: []string{"team"}},
		{name: "external, so no default variable is read", origin: registry.OriginExternal("./bin/lore-tracker"), want: []string{"team", "token"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema := generate(t, Catalog{Plugins: []registry.Entry{{Manifest: manifest, Origin: test.origin}}})

			with := dig(t, schema, "properties", "sources", "items", "allOf", 0, "then", "properties", "with")
			var required []string
			for _, key := range with["required"].([]any) {
				required = append(required, key.(string))
			}
			if !slices.Equal(required, test.want) {
				t.Errorf("with.required = %v, want %v", required, test.want)
			}
		})
	}
}

func TestARoleBindingSuggestsOnlyProvidersItCanNameDirectly(t *testing.T) {
	provider := func(name string, fields ...lore.Field) registry.Entry {
		return registry.Entry{Origin: registry.OriginBuiltin, Manifest: lore.Manifest{
			Name: name, Kind: lore.KindProvider, Capabilities: lore.Capabilities{Embed: true}, Fields: fields,
		}}
	}
	schema := generate(t, Catalog{
		Plugins: []registry.Entry{
			provider("vectors"),
			provider("gateway", lore.Field{Name: "base_url", Type: lore.FieldURL, Required: true}),
		},
		Unread: []string{"ghost"},
	})

	enum := dig(t, schema, "properties", "embedder", "properties", "provider", "anyOf", 0)["enum"]
	if got, want := enum, []any{"vectors", "ghost"}; !reflect.DeepEqual(got, want) {
		t.Errorf("embedder.provider suggests %v, want %v: a plugin with a required field needs a providers[] entry", got, want)
	}
}

func generate(t *testing.T, c Catalog) map[string]any {
	t.Helper()

	body, err := Generate(c)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	return schema
}

func dig(t *testing.T, schema map[string]any, path ...any) map[string]any {
	t.Helper()

	var at any = schema
	for _, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := at.(map[string]any)
			if !ok {
				t.Fatalf("no object at %v", path)
			}
			at = object[key]
		case int:
			list, ok := at.([]any)
			if !ok || key >= len(list) {
				t.Fatalf("no list entry at %v", path)
			}
			at = list[key]
		}
	}
	object, ok := at.(map[string]any)
	if !ok {
		t.Fatalf("no object at %v", path)
	}
	return object
}
