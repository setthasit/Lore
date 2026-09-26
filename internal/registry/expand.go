package registry

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/setthasit/Lore/internal/envx"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

// vars lists, per with: key, the variables its value expanded; none means the operator wrote a literal.
func expandWith(manifest lore.Manifest, in Instance, origin string) (
	with map[string]any, vars map[string][]string, err error) {
	secret := secretKeys(manifest)
	with = make(map[string]any, len(in.With))
	vars = make(map[string][]string, len(in.With))
	for _, key := range slices.Sorted(maps.Keys(in.With)) {
		x := withExpander{plugin: manifest.Name, allowed: origin == OriginBuiltin || secret[key]}
		expanded, err := x.expand(in.keyField(key), in.With[key])
		if err != nil {
			return nil, nil, err
		}
		with[key] = expanded
		vars[key] = x.names
	}
	return with, vars, nil
}

type withExpander struct {
	plugin  string
	allowed bool
	names   []string
}

// Containers are rebuilt, never edited, so the caller's Instance stays unexpanded.
func (x *withExpander) expand(field string, value any) (any, error) {
	switch v := value.(type) {
	case string:
		return x.leaf(field, v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			expanded, err := x.expand(field+"["+strconv.Itoa(i)+"]", item)
			if err != nil {
				return nil, err
			}
			out[i] = expanded
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			expanded, err := x.expand(field+"."+key, v[key])
			if err != nil {
				return nil, err
			}
			out[key] = expanded
		}
		return out, nil
	}
	return value, nil
}

func (x *withExpander) leaf(field, raw string) (string, error) {
	if envx.Holds(raw) && !x.allowed {
		return "", internalerror.NewBadRequestError(fmt.Sprintf(
			"%s holds an expansion, but plugin %q is installed from outside the binary, "+
				"and such a plugin expands %s only in its declared secret fields; %s",
			field, x.plugin, envx.Form, envx.EscapeRule), nil)
	}
	value, names, err := envx.ExpandNames(field, raw)
	x.names = append(x.names, names...)
	return value, err
}
