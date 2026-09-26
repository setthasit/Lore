package registry

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/envx"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

func (r *Registry) Prepare(manifest lore.Manifest, in Instance, origin string) ([]byte, map[string]string, error) {
	if err := checkRoleKeys(manifest, in); err != nil {
		return nil, nil, err
	}
	with, vars, err := expandWith(manifest, in, origin)
	if err != nil {
		return nil, nil, err
	}
	in.With = with

	if err := checkInstanceID(in); err != nil {
		return nil, nil, err
	}
	if err := checkKeys(manifest, in, vars); err != nil {
		return nil, nil, err
	}

	secrets, err := r.resolveSecrets(manifest, in, origin, vars)
	if err != nil {
		return nil, nil, err
	}

	cfg, err := configJSON(manifest, in)
	if err != nil {
		return nil, nil, err
	}
	return cfg, secrets, nil
}

func checkInstanceID(in Instance) error {
	id := in.Ident()
	if instancePattern.MatchString(id) {
		return nil
	}
	return internalerror.NewBadRequestError(fmt.Sprintf(
		"%s has id %q; %s", in.Field, id, InstanceIDRule), nil)
}

func checkRoleKeys(manifest lore.Manifest, in Instance) error {
	if in.Role == "" {
		return nil
	}
	secret := secretKeys(manifest)
	for _, key := range slices.Sorted(maps.Keys(in.With)) {
		switch {
		case secret[key]:
			continue
		case declaresField(manifest, key):
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"%s is not a secret plugin %q declares, and %s",
				in.keyField(key), manifest.Name, declareProvider(in, key)), nil)
		default:
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"%s is not a key %s accepts for plugin %q; it accepts %s",
				in.keyField(key), in.Role, manifest.Name, roleAccepts(secret)), nil)
		}
	}
	return nil
}

func declaresField(manifest lore.Manifest, key string) bool {
	return slices.ContainsFunc(manifest.Fields, func(f lore.Field) bool { return f.Name == key })
}

func declareProvider(in Instance, key string) string {
	return fmt.Sprintf("%s carries only its provider's secrets, so declare `providers: [{id: %s, use: %s}]` with %s in its with: block, "+
		"move every key %s carries besides provider, model and dimensions into that block, and keep %s naming %q",
		in.Role, in.Use, in.Use, key, in.Role, in.Field, in.Use)
}

func checkKeys(manifest lore.Manifest, in Instance, vars map[string][]string) error {
	known := make(map[string]lore.Field, len(manifest.Fields))
	for _, f := range manifest.Fields {
		known[f.Name] = f
	}
	secret := secretKeys(manifest)

	for _, key := range slices.Sorted(maps.Keys(in.With)) {
		if secret[key] {
			continue
		}
		field, ok := known[key]
		if !ok {
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"%s is not a key plugin %q accepts; it accepts %s",
				in.keyField(key), manifest.Name, accepted(manifest)), nil)
		}
		if err := checkType(in.keyField(key), fromNote(vars[key]), field, in.With[key]); err != nil {
			return err
		}
	}

	for _, f := range manifest.Fields {
		if !f.Required {
			continue
		}
		if _, set := in.With[f.Name]; set {
			continue
		}
		if in.Role != "" {
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"plugin %q requires %s%s; %s",
				manifest.Name, f.Name, doc(f.Doc), declareProvider(in, f.Name)), nil)
		}
		return internalerror.NewBadRequestError(fmt.Sprintf(
			"%s must be set%s", in.keyField(f.Name), doc(f.Doc)), nil)
	}
	return nil
}

func secretKeys(manifest lore.Manifest) map[string]bool {
	keys := make(map[string]bool, len(manifest.Secrets))
	for _, s := range manifest.Secrets {
		keys[s.Key] = true
	}
	return keys
}

func fromNote(vars []string) string {
	if len(vars) == 0 {
		return ""
	}
	names := slices.Compact(slices.Sorted(slices.Values(vars)))
	refs := make([]string, len(names))
	for i, name := range names {
		refs[i] = envx.Reference(name)
	}
	return " (from " + strings.Join(refs, ", ") + ")"
}

func doc(text string) string {
	if text == "" {
		return ""
	}
	return " — " + text
}

func accepted(manifest lore.Manifest) string {
	keys := make([]string, 0, len(manifest.Fields)+len(manifest.Secrets))
	for _, f := range manifest.Fields {
		keys = append(keys, f.Name)
	}
	for _, s := range manifest.Secrets {
		keys = append(keys, s.Key)
	}
	return listKeys(keys)
}

func listKeys(keys []string) string {
	if len(keys) == 0 {
		return "no keys at all"
	}
	slices.Sort(keys)
	return strings.Join(keys, ", ")
}

var roleBindingKeys = []string{"provider", "model", "dimensions"}

func roleAccepts(secret map[string]bool) string {
	return strings.Join(slices.Concat(roleBindingKeys, slices.Sorted(maps.Keys(secret))), ", ")
}

// from notes the variables behind the whole value; a non-string list item was never expanded, so it gets none.
func checkType(field, from string, declared lore.Field, value any) error {
	whole := field + from
	switch declared.Type {
	case lore.FieldString:
		if _, ok := value.(string); !ok {
			return typeError(whole, "a string")
		}
	case lore.FieldURL:
		raw, ok := value.(string)
		if !ok {
			return typeError(whole, "an absolute http(s) URL")
		}
		return CheckURL(whole, raw, declared.Default)
	case lore.FieldInt:
		if !integral(value) {
			return typeError(whole, "a whole number")
		}
	case lore.FieldBool:
		if _, ok := value.(bool); !ok {
			return typeError(whole, "true or false")
		}
	case lore.FieldStringList:
		items, ok := value.([]any)
		if !ok {
			return typeError(whole, "a list of strings")
		}
		if declared.Required && len(items) == 0 {
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"%s must list at least one entry: an empty list selects nothing, so this instance would ingest nothing%s",
				field, doc(declared.Doc)), nil)
		}
		for i, item := range items {
			if _, ok := item.(string); !ok {
				return typeError(field+"["+strconv.Itoa(i)+"]", "a string")
			}
		}
	case lore.FieldDuration:
		raw, ok := value.(string)
		if !ok || !isDuration(raw) {
			return typeError(whole, `a duration like "30m" or "30d"`)
		}
	}
	return nil
}

func isDuration(raw string) bool {
	_, err := lore.ParseDuration(raw)
	return err == nil
}

func CheckURL(field, raw, example string) error {
	parsed, err := url.Parse(raw)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
		return nil
	}
	want := "an absolute http(s) URL"
	if example != "" {
		want += " like " + example
	}
	return typeError(field, want)
}

// A JSON decoder yields float64 where YAML yields int; both are whole numbers.
func integral(value any) bool {
	switch n := value.(type) {
	case int:
		return true
	case int64:
		return true
	case float64:
		return n == float64(int64(n))
	}
	return false
}

func typeError(field, want string) error {
	return internalerror.NewBadRequestError(field+" must be "+want, nil)
}

func configJSON(manifest lore.Manifest, in Instance) ([]byte, error) {
	declared := make(map[string]any, len(manifest.Fields))
	for _, f := range manifest.Fields {
		if value, set := in.With[f.Name]; set {
			declared[f.Name] = value
		}
	}

	raw, err := json.Marshal(declared)
	if err != nil {
		return nil, internalerror.NewInternalError(fmt.Sprintf(
			"cannot encode the configuration of %s for plugin %q", in.Field, manifest.Name), err)
	}
	return raw, nil
}
