package registry

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

// Instance is a configured plugin instance, independent of the file format it
// was read from: the registry never parses YAML, so the same code serves a
// configuration file and a test table.
type Instance struct {
	// ID is the instance identity — sync cursor key, Document.Source and DocID
	// prefix. It defaults to Use.
	ID string

	// Use names the plugin.
	Use string

	// With is the decoded `with:` block; a nil map is an empty block.
	With map[string]any

	// Field is the configuration path this instance was read from, quoted
	// verbatim in errors so a message points at a line the operator can edit.
	Field string
}

// Binding is a role bound to a provider instance and a model.
type Binding struct {
	Provider   string
	Model      string
	Dimensions int
	Capability lore.Capability

	// Field is the configuration path of the role, "embedder" or "llm".
	Field string
}

// Clone is a registered local clone a code plugin is bound to.
type Clone struct {
	Path   string
	Use    string
	Remote string
	Field  string
}

// Code pairs a built code accessor with the clone it reads.
type Code struct {
	Path   string
	Remote string
	Repo   lore.CodeRepo
}

// BuildSources builds one connector per instance, in configuration order,
// because sync order is the order the operator wrote.
func (r *Registry) BuildSources(instances []Instance) ([]lore.Connector, error) {
	connectors := make([]lore.Connector, 0, len(instances))
	for _, in := range instances {
		conn, err := r.buildSource(in)
		if err != nil {
			return nil, err
		}
		connectors = append(connectors, conn)
	}
	return connectors, nil
}

func (r *Registry) buildSource(in Instance) (lore.Connector, error) {
	id, plugin, manifest, err := r.resolve(in, lore.KindSource)
	if err != nil {
		return nil, err
	}

	cfg, secrets, err := r.prepare(manifest, in)
	if err != nil {
		return nil, err
	}

	conn, err := plugin.(lore.SourcePlugin).NewSource(lore.SourceConfig{
		Instance: id,
		Config:   cfg,
		Secrets:  secrets,
		Host:     r.instanceHost(id),
	})
	if err != nil {
		return nil, unbuildable(in.Field, in.Use, err)
	}
	if conn == nil {
		return nil, internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q built no connector for %s and reported no error", in.Use, in.Field), nil)
	}
	if name := conn.Name(); name != id {
		return nil, internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q built a connector calling itself %q for instance %q; the instance id is the cursor key and the document namespace, so a connector may not rename itself",
			in.Use, name, id), nil)
	}
	// A source that claims its documents carry repository paths must be able to
	// answer which remotes it ingests, or the startup warning about an unmatched
	// local clone silently stops working.
	if _, ok := conn.(lore.RemoteMatcher); manifest.Capabilities.RepoRemotes && !ok {
		return nil, internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q declares repo_remotes but its connector does not implement MatchesRemote", in.Use), nil)
	}
	return conn, nil
}

// Provider is a built provider together with the two names the host needs from
// it: the plugin, which is the first component of the vector-space identity the
// host composes, and the instance the operator configured.
type Provider struct {
	Plugin   string
	Instance string
	Value    lore.Provider
}

// BuildProvider builds the provider a role binds to. An id that names no
// declared instance but does name a registered plugin is built with that
// plugin's defaults, which is what keeps a two-line starter configuration
// working without a providers: block.
func (r *Registry) BuildProvider(b Binding, instances []Instance) (Provider, error) {
	if b.Provider == "" {
		return Provider{}, internalerror.NewBadRequestError(b.Field+".provider must name a provider", nil)
	}
	if b.Model == "" {
		return Provider{}, internalerror.NewBadRequestError(b.Field+".model must name a model", nil)
	}

	in, declared := findInstance(instances, b.Provider)
	if !declared {
		if _, known := r.entries[b.Provider]; !known {
			return Provider{}, r.unresolved(b.Field+".provider", b.Provider, lore.KindProvider, instances)
		}
		in = Instance{Use: b.Provider, Field: b.Field + ".provider"}
	}

	id, plugin, manifest, err := r.resolve(in, lore.KindProvider)
	if err != nil {
		return Provider{}, err
	}
	if !manifest.Capabilities.Declares(b.Capability) {
		return Provider{}, internalerror.NewBadRequestError(fmt.Sprintf(
			"%s binds provider %q, which does not serve %s; %s", b.Field, id, b.Capability, serves(manifest)), nil)
	}

	cfg, secrets, err := r.prepare(manifest, in)
	if err != nil {
		return Provider{}, err
	}

	built, err := plugin.(lore.ProviderPlugin).NewProvider(lore.ProviderConfig{
		Instance:   id,
		Capability: b.Capability,
		Model:      b.Model,
		Dimensions: b.Dimensions,
		Config:     cfg,
		Secrets:    secrets,
		Host:       r.instanceHost(id),
	})
	if err != nil {
		return Provider{}, internalerror.NewBadRequestError(fmt.Sprintf(
			"cannot configure %s from provider %q: %s", b.Field, id, err.Error()), err)
	}
	if err := assertCapability(b, id, manifest, built); err != nil {
		return Provider{}, err
	}
	return Provider{Plugin: manifest.Name, Instance: id, Value: built}, nil
}

// A manifest that claims a capability the built value does not implement would
// otherwise surface as a nil-interface panic on the first query.
func assertCapability(b Binding, id string, manifest lore.Manifest, built lore.Provider) error {
	var ok bool
	switch b.Capability {
	case lore.CapabilityEmbed:
		_, ok = built.(lore.Embedder)
	case lore.CapabilityComplete:
		_, ok = built.(lore.Completer)
	default:
		return internalerror.NewInternalError(fmt.Sprintf(
			"%s asked provider %q for unknown capability %q", b.Field, id, b.Capability), nil)
	}
	if !ok {
		return internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q declares %s but built a value that does not implement it", manifest.Name, b.Capability), nil)
	}
	return nil
}

func serves(manifest lore.Manifest) string {
	names := manifest.Capabilities.Names()
	if len(names) == 0 {
		return "it serves no model capability at all"
	}

	served := make([]string, 0, len(names))
	for _, n := range names {
		served = append(served, string(n))
	}
	return "it serves " + strings.Join(served, " and ")
}

// BuildCode builds one accessor per registered clone. Root is already absolute:
// path expansion is the configuration's job, not a plugin's.
func (r *Registry) BuildCode(clones []Clone) ([]Code, error) {
	out := make([]Code, 0, len(clones))
	for _, clone := range clones {
		in := Instance{Use: clone.Use, Field: clone.Field}
		_, plugin, _, err := r.resolve(in, lore.KindCode)
		if err != nil {
			return nil, err
		}

		repo, err := plugin.(lore.CodePlugin).NewCode(lore.CodeConfig{
			Root:   clone.Path,
			Remote: clone.Remote,
			Host:   r.instanceHost(clone.Use),
		})
		if err != nil {
			return nil, unbuildable(clone.Field, clone.Use, err)
		}
		out = append(out, Code{Path: clone.Path, Remote: clone.Remote, Repo: repo})
	}
	return out, nil
}

// Warnings are configuration facts that degrade answers without being errors.
// It is a named type so the wiring can inject it without colliding with every
// other list of strings in the graph.
type Warnings []string

// UnmatchedRemotes reports clones whose remote no ingesting source claims. It
// asks the connectors rather than switching on a forge name, so a third-party
// forge plugin keeps the warning working by implementing lore.RemoteMatcher.
func UnmatchedRemotes(clones []Clone, sources []lore.Connector) Warnings {
	var warnings Warnings
	for _, clone := range clones {
		if clone.Remote == "" || ingested(sources, clone.Remote) {
			continue
		}
		warnings = append(warnings, "repos path "+clone.Path+" has remote "+clone.Remote+
			", which names no configured source repo — blame still works, but chains stop at the commit layer")
	}
	return warnings
}

func ingested(sources []lore.Connector, remote string) bool {
	for _, source := range sources {
		if matcher, ok := source.(lore.RemoteMatcher); ok && matcher.MatchesRemote(remote) {
			return true
		}
	}
	return false
}

func findInstance(instances []Instance, id string) (Instance, bool) {
	for _, in := range instances {
		if in.Ident() == id {
			return in, true
		}
	}
	return Instance{}, false
}

// Ident is the instance identity, defaulting to the plugin name so a
// single-instance workspace reads `use: github` and gets the id "github".
func (in Instance) Ident() string {
	if in.ID != "" {
		return in.ID
	}
	return in.Use
}

func (r *Registry) resolve(in Instance, kind lore.Kind) (string, lore.Plugin, lore.Manifest, error) {
	id := in.Ident()
	if !instancePattern.MatchString(id) {
		return "", nil, lore.Manifest{}, internalerror.NewBadRequestError(fmt.Sprintf(
			"%s has id %q; an instance id becomes the prefix of every document identity it produces, so it must start with a letter or digit and hold only letters, digits, - and _",
			in.Field, id), nil)
	}

	entry, known := r.entries[in.Use]
	if !known {
		return "", nil, lore.Manifest{}, r.unresolved(in.Field+".use", in.Use, kind, nil)
	}
	if entry.Manifest.Kind != kind {
		return "", nil, lore.Manifest{}, internalerror.NewBadRequestError(fmt.Sprintf(
			"%s.use names %q, which is a %s plugin, not a %s plugin", in.Field, in.Use, entry.Manifest.Kind, kind), nil)
	}
	return id, r.plugins[in.Use], entry.Manifest, nil
}

// The failure names what this build actually has, because the fix depends on
// whether the plugin exists at all or merely is not compiled in.
func (r *Registry) unresolved(field, name string, kind lore.Kind, instances []Instance) error {
	compiled := r.Names(kind)
	have := "nothing of that kind"
	if len(compiled) > 0 {
		have = strings.Join(compiled, ", ")
	}

	message := fmt.Sprintf("%s names %q, which is neither a compiled plugin (%s) nor declared in plugins:. "+
		"Either add it to plugins: as an external binary, or build a lore binary that registers it. "+
		"Run `lore plugin list` to see what this build has.", field, name, have)
	if kind == lore.KindProvider && len(instances) > 0 {
		message += " Declared provider instances: " + strings.Join(instanceIdents(instances), ", ") + "."
	}
	return internalerror.NewBadRequestError(message, nil)
}

func instanceIdents(instances []Instance) []string {
	out := make([]string, 0, len(instances))
	for _, in := range instances {
		out = append(out, in.Ident())
	}
	slices.Sort(out)
	return out
}

func unbuildable(field, use string, err error) error {
	return internalerror.NewBadRequestError(fmt.Sprintf(
		"cannot configure %s from plugin %q: %s", field, use, err.Error()), err)
}

// Host is what a plugin this registry builds is lent, tagged for one instance.
// It is exported so the wiring can hand the identical Host to an external
// plugin: neither mode gets a privilege the other lacks.
func (r *Registry) Host(instance string) lore.Host { return r.instanceHost(instance) }

// A plugin's logger is tagged with the instance so interleaved rounds stay
// readable, and its clock is injected so time-dependent behavior stays testable.
func (r *Registry) instanceHost(instance string) lore.Host {
	host := r.host
	if host.Log != nil {
		host.Log = host.Log.With(slog.String("instance", instance))
	}
	return host
}

// prepare turns a `with:` block into the two things a plugin is built from: the
// configuration JSON it decodes itself, and the resolved secrets the host
// injects. A plugin never reads the environment, so this is the only place an
// environment variable is looked up on its behalf.
func (r *Registry) prepare(manifest lore.Manifest, in Instance) ([]byte, map[string]string, error) {
	if err := checkKeys(manifest, in); err != nil {
		return nil, nil, err
	}

	secrets, err := resolveSecrets(manifest, in)
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

	for _, key := range sortedKeys(in.With) {
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

// Keys are visited in a fixed order so a configuration with two mistakes always
// reports the same one first.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
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
		return checkURL(field, raw, declared.Default)
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

// An unset optional URL means the plugin's own default, so only a value that is
// present has to be one a request can actually be built from.
func checkURL(field, raw, example string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return internalerror.NewBadRequestError(field+" is not a URL: "+raw, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		want := "an absolute http(s) URL"
		if example != "" {
			want += " like " + example
		}
		return internalerror.NewBadRequestError(fmt.Sprintf("%s must be %s, got %s", field, want, raw), nil)
	}
	return nil
}

// YAML resolves an integer to int, but a value that arrived through JSON is a
// float64; both are whole numbers and both are accepted.
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

func resolveSecrets(manifest lore.Manifest, in Instance) (map[string]string, error) {
	if len(manifest.Secrets) == 0 {
		return nil, nil
	}

	secrets := make(map[string]string, len(manifest.Secrets))
	for _, s := range manifest.Secrets {
		name := s.DefaultEnv
		if declared, set := in.With[s.ConfigField]; set {
			named, ok := declared.(string)
			if !ok || named == "" {
				return nil, internalerror.NewBadRequestError(fmt.Sprintf(
					"%s.with.%s must name an environment variable", in.Field, s.ConfigField), nil)
			}
			name = named
		}
		if name == "" {
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s must name the environment variable holding the %s", in.Field, s.ConfigField, secretDoc(s)), nil)
		}
		if !envPattern.MatchString(name) {
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s must be an environment variable name, got %s", in.Field, s.ConfigField, name), nil)
		}

		value := os.Getenv(name)
		if value == "" {
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s names %s, but that environment variable is not set", in.Field, s.ConfigField, name), nil)
		}
		secrets[s.Key] = value
	}
	return secrets, nil
}

func secretDoc(s lore.Secret) string {
	if s.Doc != "" {
		return s.Doc
	}
	return s.Key
}

// The keys that name environment variables are stripped: a plugin receives
// resolved values under its own secret keys and never sees the operator's
// variable names, which is what keeps its key names independent of them.
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
