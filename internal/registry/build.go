package registry

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

type Instance struct {
	// Sync cursor key, Document.Source and DocID prefix; empty means Use.
	ID string

	Use string

	With map[string]any

	// Configuration path, quoted verbatim in errors.
	Field string
}

type Binding struct {
	Provider   string
	Model      string
	Dimensions int
	Capability lore.Capability

	// Configuration path of the role, "embedder" or "llm".
	Field string
}

type LocalClone struct {
	Path   string
	Use    string
	Remote string
	Field  string
}

type Code struct {
	Path   string
	Remote string
	Repo   lore.CodeRepo
}

// One connector per instance, in configuration order.
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
	id, plugin, manifest, origin, err := r.resolve(in, lore.KindSource)
	if err != nil {
		return nil, err
	}

	cfg, secrets, err := prepare(manifest, in, origin)
	if err != nil {
		return nil, err
	}

	conn, err := plugin.(lore.SourcePlugin).NewSource(lore.SourceConfig{
		Instance: id,
		Config:   cfg,
		Secrets:  secrets,
		Host:     r.Host(id),
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
	// Without MatchesRemote the unmatched-clone startup warning silently stops working.
	if _, ok := conn.(lore.RemoteMatcher); manifest.Capabilities.RepoRemotes && !ok {
		return nil, internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q declares repo_remotes but its connector does not implement MatchesRemote", in.Use), nil)
	}
	return conn, nil
}

type BuiltProvider struct {
	// First component of the vector-space identity.
	Plugin   string
	Instance string
	Value    lore.Provider
}

// An id that names no declared instance but does name a registered plugin is
// built from that plugin's defaults.
func (r *Registry) BuildProvider(b Binding, instances []Instance) (BuiltProvider, error) {
	if b.Provider == "" {
		return BuiltProvider{}, internalerror.NewBadRequestError(b.Field+".provider must name a provider", nil)
	}
	if b.Model == "" {
		return BuiltProvider{}, internalerror.NewBadRequestError(b.Field+".model must name a model", nil)
	}

	in, declared := findInstance(instances, b.Provider)
	if !declared {
		if _, known := r.entries[b.Provider]; !known {
			return BuiltProvider{}, r.unresolved(b.Field+".provider", b.Provider, lore.KindProvider, instances)
		}
		in = Instance{Use: b.Provider, Field: b.Field + ".provider"}
	}

	id, plugin, manifest, origin, err := r.resolve(in, lore.KindProvider)
	if err != nil {
		return BuiltProvider{}, err
	}
	if !manifest.Capabilities.Declares(b.Capability) {
		return BuiltProvider{}, internalerror.NewBadRequestError(fmt.Sprintf(
			"%s binds provider %q, which does not serve %s; %s", b.Field, id, b.Capability, serves(manifest)), nil)
	}

	cfg, secrets, err := prepare(manifest, in, origin)
	if err != nil {
		return BuiltProvider{}, err
	}

	built, err := plugin.(lore.ProviderPlugin).NewProvider(lore.ProviderConfig{
		Instance:   id,
		Capability: b.Capability,
		Model:      b.Model,
		Dimensions: b.Dimensions,
		Config:     cfg,
		Secrets:    secrets,
		Host:       r.Host(id),
	})
	if err != nil {
		return BuiltProvider{}, unbuildable(b.Field, id, err)
	}
	if err := assertCapability(b, id, manifest, built); err != nil {
		return BuiltProvider{}, err
	}
	return BuiltProvider{Plugin: manifest.Name, Instance: id, Value: built}, nil
}

func assertCapability(b Binding, id string, manifest lore.Manifest, built lore.Provider) error {
	var ok bool
	switch b.Capability {
	case lore.CapabilityEmbed:
		_, ok = built.(lore.Embedder)
	case lore.CapabilityComplete:
		_, ok = built.(lore.Completer)
	}
	if !ok {
		return internalerror.NewInternalError(fmt.Sprintf(
			"plugin %q declares %s but built a value that does not implement it", manifest.Name, b.Capability), nil)
	}
	return nil
}

func serves(manifest lore.Manifest) string {
	if len(manifest.Capabilities.Names()) == 0 {
		return "it serves no model capability at all"
	}
	return "it serves " + manifest.Capabilities.String()
}

// clone.Path must already be absolute; plugins never expand paths.
func (r *Registry) BuildCode(clones []LocalClone) ([]Code, error) {
	out := make([]Code, 0, len(clones))
	for _, clone := range clones {
		in := Instance{Use: clone.Use, Field: clone.Field}
		_, plugin, _, _, err := r.resolve(in, lore.KindCode)
		if err != nil {
			return nil, err
		}

		repo, err := plugin.(lore.CodePlugin).NewCode(lore.CodeConfig{
			Root: clone.Path,
			Host: r.Host(clone.Use),
		})
		if err != nil {
			return nil, unbuildable(clone.Field, clone.Use, err)
		}
		if repo == nil {
			return nil, internalerror.NewInternalError(fmt.Sprintf(
				"plugin %q built no accessor for clone %s and reported no error", clone.Use, clone.Path), nil)
		}
		out = append(out, Code{Path: clone.Path, Remote: clone.Remote, Repo: repo})
	}
	return out, nil
}

func findInstance(instances []Instance, id string) (Instance, bool) {
	for _, in := range instances {
		if in.Ident() == id {
			return in, true
		}
	}
	return Instance{}, false
}

func (in Instance) Ident() string {
	if in.ID != "" {
		return in.ID
	}
	return in.Use
}

func (r *Registry) resolve(in Instance, kind lore.Kind) (string, lore.Plugin, lore.Manifest, string, error) {
	id := in.Ident()
	if !instancePattern.MatchString(id) {
		return "", nil, lore.Manifest{}, "", internalerror.NewBadRequestError(fmt.Sprintf(
			"%s has id %q; an instance id becomes the prefix of every document identity it produces, so it must start with a letter or digit and hold only letters, digits, - and _",
			in.Field, id), nil)
	}

	entry, known := r.entries[in.Use]
	if !known {
		return "", nil, lore.Manifest{}, "", r.unresolved(in.Field+".use", in.Use, kind, nil)
	}
	if entry.Manifest.Kind != kind {
		return "", nil, lore.Manifest{}, "", internalerror.NewBadRequestError(fmt.Sprintf(
			"%s.use names %q, which is a %s plugin, not a %s plugin", in.Field, in.Use, entry.Manifest.Kind, kind), nil)
	}
	return id, r.plugins[in.Use], entry.Manifest, entry.Origin, nil
}

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

func (r *Registry) Host(instance string) lore.Host {
	host := r.host
	if host.Log == nil {
		host.Log = slog.New(slog.DiscardHandler)
	}
	host.Log = host.Log.With(slog.String("instance", instance))
	return host
}
