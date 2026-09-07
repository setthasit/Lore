// Package registry maps the plugin names in a workspace configuration to the
// connectors, providers and code accessors the engine runs on.
package registry

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
	"maps"
)

const OriginBuiltin = "builtin"

func OriginExternal(binary string) string {
	return "external " + binary
}

// A plugin name is also a directory name in the plugin cache, so a separator or a dot must never match.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// An instance id becomes the DocID prefix, so a colon would make document identities unparseable.
var instancePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func ValidPluginName(name string) bool {
	return namePattern.MatchString(name)
}

type Entry struct {
	Manifest lore.Manifest
	Origin   string
}

type Registry struct {
	host    lore.Host
	entries map[string]Entry
	plugins map[string]lore.Plugin
	order   []string
}

func New(host lore.Host) *Registry {
	return &Registry{
		host:    host,
		entries: make(map[string]Entry),
		plugins: make(map[string]lore.Plugin),
	}
}

type Compiled struct{ *Registry }

func (r *Registry) Clone() *Registry {
	out := &Registry{
		host:    r.host,
		entries: make(map[string]Entry, len(r.entries)),
		plugins: make(map[string]lore.Plugin, len(r.plugins)),
		order:   slices.Clone(r.order),
	}
	maps.Copy(out.entries, r.entries)
	maps.Copy(out.plugins, r.plugins)
	return out
}

func (r *Registry) Register(plugins ...lore.Plugin) error {
	return r.register(OriginBuiltin, plugins...)
}

func (r *Registry) RegisterExternal(origin, name string, p lore.Plugin) error {
	if got := p.Manifest().Name; got != name {
		return internalerror.NewBadRequestError(fmt.Sprintf(
			"plugins[%s] is a binary whose manifest calls itself %q; rename the declaration or the plugin",
			name, got), nil)
	}
	return r.register(origin, p)
}

func (r *Registry) register(origin string, plugins ...lore.Plugin) error {
	for _, p := range plugins {
		if p == nil {
			return internalerror.NewInternalError("a nil plugin was registered", nil)
		}

		m := p.Manifest()
		if err := validateManifest(m, p); err != nil {
			return err
		}
		if existing, taken := r.entries[m.Name]; taken {
			return internalerror.NewBadRequestError(fmt.Sprintf(
				"plugin %q is registered twice (%s and %s); every plugin name must be unique because `use:` resolves by name",
				m.Name, existing.Origin, origin), nil)
		}

		r.entries[m.Name] = Entry{Manifest: m, Origin: origin}
		r.plugins[m.Name] = p
		r.order = append(r.order, m.Name)
	}
	return nil
}

func (r *Registry) Manifest(name string) (lore.Manifest, bool) {
	entry, ok := r.entries[name]
	return entry.Manifest, ok
}

// Ordered by name.
func (r *Registry) List() []Entry {
	out := make([]Entry, 0, len(r.entries))
	for _, name := range r.order {
		out = append(out, r.entries[name])
	}
	slices.SortFunc(out, func(a, b Entry) int { return cmp.Compare(a.Manifest.Name, b.Manifest.Name) })
	return out
}

// Sorted by name.
func (r *Registry) Names(kind lore.Kind) []string {
	var out []string
	for name, entry := range r.entries {
		if entry.Manifest.Kind == kind {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Starter reports the first plugin of kind that serves want, in registration
// order; an empty want matches any.
func (r *Registry) Starter(kind lore.Kind, want lore.Capability) (lore.Manifest, bool) {
	for _, name := range r.order {
		m := r.entries[name].Manifest
		if m.Kind != kind {
			continue
		}
		if want == "" || m.Capabilities.Declares(want) {
			return m, true
		}
	}
	return lore.Manifest{}, false
}

func validateManifest(m lore.Manifest, p lore.Plugin) error {
	if m.Name == "" {
		return internalerror.NewInternalError("a plugin declares no name, so nothing in a configuration could refer to it", nil)
	}
	if !ValidPluginName(m.Name) {
		return internalerror.NewInternalError(fmt.Sprintf(
			"plugin name %q must be lower-case and hyphenated, like \"openai-compatible\"", m.Name), nil)
	}
	if m.APIVersion != lore.APIVersion {
		return internalerror.NewPreconditionError(fmt.Sprintf(
			"plugin %q speaks api_version %d, host speaks %d", m.Name, m.APIVersion, lore.APIVersion), nil)
	}
	if m.Summary == "" {
		return internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q declares no summary; `lore plugin list` has nothing to show for it", m.Name), nil)
	}
	if err := validateKind(m, p); err != nil {
		return err
	}
	if err := validateCapabilities(m); err != nil {
		return err
	}
	return validateFields(m)
}

func validateKind(m lore.Manifest, p lore.Plugin) error {
	var ok bool
	switch m.Kind {
	case lore.KindSource:
		_, ok = p.(lore.SourcePlugin)
	case lore.KindProvider:
		_, ok = p.(lore.ProviderPlugin)
	case lore.KindCode:
		_, ok = p.(lore.CodePlugin)
	default:
		return internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q declares kind %q; the kinds are %s", m.Name, m.Kind,
			joinKinds(lore.KindSource, lore.KindProvider, lore.KindCode)), nil)
	}
	if !ok {
		return internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q declares kind %q but does not implement the matching constructor", m.Name, m.Kind), nil)
	}
	return nil
}

func validateCapabilities(m lore.Manifest) error {
	caps := m.Capabilities
	if caps.RepoRemotes && m.Kind != lore.KindSource {
		return internalerror.NewInternalError(fmt.Sprintf(
			"%s plugin %q declares repo_remotes, which only a source can serve", m.Kind, m.Name), nil)
	}
	for _, capability := range slices.Sorted(maps.Keys(m.DefaultModels)) {
		if !caps.Declares(capability) {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q suggests a default model for %s, a capability it does not declare, so no role could ever ask for it",
				m.Name, capability), nil)
		}
	}
	if m.Kind == lore.KindProvider {
		if !caps.Embed && !caps.Complete {
			return internalerror.NewInternalError(fmt.Sprintf(
				"provider plugin %q declares neither embed nor complete, so no role could bind to it", m.Name), nil)
		}
		return nil
	}
	if caps.Embed || caps.Complete {
		return internalerror.NewInternalError(fmt.Sprintf(
			"%s plugin %q declares a model capability, which only a provider can serve", m.Kind, m.Name), nil)
	}
	return nil
}

func validateFields(m lore.Manifest) error {
	seen := make(map[string]string, len(m.Fields)+len(m.Secrets))

	for _, f := range m.Fields {
		if !keyPattern.MatchString(f.Name) {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q declares field %q; a field name must be snake_case", m.Name, f.Name), nil)
		}
		if !validFieldType(f.Type) {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q declares field %q with type %q; the types are %s",
				m.Name, f.Name, f.Type, strings.Join(fieldTypeNames(), ", ")), nil)
		}
		if where, taken := seen[f.Name]; taken {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q declares %q twice (%s and field)", m.Name, f.Name, where), nil)
		}
		seen[f.Name] = "field"
	}

	for _, s := range m.Secrets {
		if !keyPattern.MatchString(s.Key) {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q declares secret key %q; a secret key must be snake_case", m.Name, s.Key), nil)
		}
		if !keyPattern.MatchString(s.ConfigField) || !strings.HasSuffix(s.ConfigField, "_env") {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q declares secret %q with config field %q; it must be snake_case and end in _env, because it names an environment variable rather than holding a value",
				m.Name, s.Key, s.ConfigField), nil)
		}
		if s.DefaultEnv != "" && !envPattern.MatchString(s.DefaultEnv) {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q declares secret %q with default env %q, which is not an environment variable name",
				m.Name, s.Key, s.DefaultEnv), nil)
		}
		if where, taken := seen[s.ConfigField]; taken {
			return internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q declares %q twice (%s and secret config field)", m.Name, s.ConfigField, where), nil)
		}
		seen[s.ConfigField] = "secret config field"
	}
	return nil
}

func validFieldType(t lore.FieldType) bool {
	return slices.Contains(fieldTypes(), t)
}

func fieldTypes() []lore.FieldType {
	return []lore.FieldType{
		lore.FieldString, lore.FieldURL, lore.FieldInt,
		lore.FieldBool, lore.FieldStringList, lore.FieldDuration,
	}
}

func fieldTypeNames() []string {
	names := make([]string, 0, len(fieldTypes()))
	for _, t := range fieldTypes() {
		names = append(names, string(t))
	}
	return names
}

func joinKinds(kinds ...lore.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}
