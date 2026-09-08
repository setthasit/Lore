package registry

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/urlx"
	"github.com/setthasit/Lore/sdk"
)

func prepare(manifest lore.Manifest, in Instance, origin string) ([]byte, map[string]string, error) {
	if err := checkKeys(manifest, in); err != nil {
		return nil, nil, err
	}

	secrets, err := resolveSecrets(manifest, in, origin)
	if err != nil {
		return nil, nil, err
	}

	cfg, err := configJSON(manifest, in)
	if err != nil {
		return nil, nil, err
	}
	return cfg, secrets, nil
}

func checkKeys(manifest lore.Manifest, in Instance) error {
	known := make(map[string]lore.Field, len(manifest.Fields))
	for _, f := range manifest.Fields {
		known[f.Name] = f
	}
	secretFields := make(map[string]struct{}, len(manifest.Secrets))
	for _, s := range manifest.Secrets {
		secretFields[s.ConfigField] = struct{}{}
	}

	for _, key := range slices.Sorted(maps.Keys(in.With)) {
		if _, ok := secretFields[key]; ok {
			continue
		}
		field, ok := known[key]
		if !ok {
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s is not a key plugin %q accepts; it accepts %s",
				in.Field, key, manifest.Name, accepted(manifest)), nil)
		}
		if err := checkType(in.Field+".with."+key, field, in.With[key]); err != nil {
			return err
		}
	}

	for _, f := range manifest.Fields {
		if !f.Required {
			continue
		}
		if _, set := in.With[f.Name]; !set {
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s must be set%s", in.Field, f.Name, doc(f.Doc)), nil)
		}
	}
	return nil
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
		keys = append(keys, s.ConfigField)
	}
	if len(keys) == 0 {
		return "no keys at all"
	}
	slices.Sort(keys)
	return strings.Join(keys, ", ")
}

func checkType(field string, declared lore.Field, value any) error {
	switch declared.Type {
	case lore.FieldString:
		if _, ok := value.(string); !ok {
			return typeError(field, "a string", value)
		}
	case lore.FieldURL:
		raw, ok := value.(string)
		if !ok {
			return typeError(field, "an absolute http(s) URL", value)
		}
		return CheckURL(field, raw, declared.Default)
	case lore.FieldInt:
		if !integral(value) {
			return typeError(field, "a whole number", value)
		}
	case lore.FieldBool:
		if _, ok := value.(bool); !ok {
			return typeError(field, "true or false", value)
		}
	case lore.FieldStringList:
		items, ok := value.([]any)
		if !ok {
			return typeError(field, "a list of strings", value)
		}
		for i, item := range items {
			if _, ok := item.(string); !ok {
				return typeError(field+"["+strconv.Itoa(i)+"]", "a string", item)
			}
		}
	case lore.FieldDuration:
		raw, ok := value.(string)
		if !ok {
			return typeError(field, `a duration like "30m" or "30d"`, value)
		}
		if _, err := lore.ParseDuration(raw); err != nil {
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"%s is not a duration: %s", field, raw), err)
		}
	}
	return nil
}

func CheckURL(field, raw, example string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return internalerror.NewBadRequestError(field+" is not a URL", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		want := "an absolute http(s) URL"
		if example != "" {
			want += " like " + example
		}
		return internalerror.NewBadRequestError(fmt.Sprintf("%s must be %s, got %s", field, want, urlx.Redact(parsed)), nil)
	}
	return nil
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

func typeError(field, want string, got any) error {
	return internalerror.NewBadRequestError(fmt.Sprintf(
		"%s must be %s, got %v", field, want, got), nil)
}

// Secret config fields stay out: a plugin gets values under Secret.Key, never the operator's variable name.
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
