package lore

import (
	"slices"
	"strings"
)

// APIVersion mismatch is fatal: the host refuses the plugin, because indexing
// over a contract the two sides disagree on corrupts the index beyond repair.
const APIVersion = 1

type Kind string

const (
	KindSource   Kind = "source"
	KindProvider Kind = "provider"
	KindCode     Kind = "code"
)

type Plugin interface {
	Manifest() Manifest
}

type SourcePlugin interface {
	Plugin
	NewSource(SourceConfig) (Connector, error)
}

type ProviderPlugin interface {
	Plugin
	NewProvider(ProviderConfig) (Provider, error)
}

type CodePlugin interface {
	Plugin
	NewCode(CodeConfig) (CodeRepo, error)
}

// Provider satisfies Embedder, Completer or both, matching the capabilities its
// manifest declares; the host asserts it against them.
type Provider = any

// RemoteMatcher is the optional interface a Connector declaring
// Capabilities.RepoRemotes implements; remote is "<forge>:<namespace>/<name>".
type RemoteMatcher interface {
	MatchesRemote(remote string) bool
}

func SplitRemote(remote string) (forge, path string, ok bool) {
	forge, path, ok = strings.Cut(remote, ":")
	return forge, path, ok && forge != "" && IsNamespacedPath(path)
}

func IsNamespacedPath(path string) bool {
	segments := strings.Split(path, "/")
	return len(segments) >= 2 && !slices.Contains(segments, "")
}

type Manifest struct {
	Name         string       `json:"name"`
	Kind         Kind         `json:"kind"`
	APIVersion   int          `json:"api_version"`
	Summary      string       `json:"summary"` // one line; shown by `lore plugin list`
	Capabilities Capabilities `json:"capabilities"`
	Fields       []Field      `json:"fields"` // the host rejects a `with:` key not declared here
	Secrets      []Secret     `json:"secrets"`

	// DefaultModels suggests one model per capability for scaffolds and prompts;
	// the host never applies a suggestion.
	DefaultModels map[Capability]string `json:"default_models,omitempty"`
}

type Capabilities struct {
	Embed       bool `json:"embed"`
	Complete    bool `json:"complete"`
	RepoRemotes bool `json:"repo_remotes"` // documents carry repo paths a local clone maps onto
}

type Capability string

const (
	CapabilityEmbed    Capability = "embed"
	CapabilityComplete Capability = "complete"
)

func (c Capabilities) Declares(want Capability) bool {
	switch want {
	case CapabilityEmbed:
		return c.Embed
	case CapabilityComplete:
		return c.Complete
	}
	return false
}

// Names lists the capabilities declared, in the order errors report them.
func (c Capabilities) Names() []Capability {
	var out []Capability
	if c.Embed {
		out = append(out, CapabilityEmbed)
	}
	if c.Complete {
		out = append(out, CapabilityComplete)
	}
	return out
}

func (c Capabilities) String() string {
	names := c.Names()
	if len(names) == 0 {
		return "nothing"
	}

	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return strings.Join(out, ", ")
}

type FieldType string

const (
	FieldString     FieldType = "string"
	FieldURL        FieldType = "url"
	FieldInt        FieldType = "int"
	FieldBool       FieldType = "bool"
	FieldStringList FieldType = "string_list"
	FieldDuration   FieldType = "duration"
)

// Field is one key of a plugin's `with:` block.
type Field struct {
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`

	// Default documents the fallback value; the host never injects it, because
	// only the plugin knows the value's real type.
	Default string `json:"default,omitempty"`

	Doc    string `json:"doc,omitempty"`    // scaffold comment and required-field error suffix
	Prompt string `json:"prompt,omitempty"` // question `lore source add` asks; defaults to Name
}

// Secret is a credential the host injects: the plugin names Key, the operator
// names the env var holding it in ConfigField. A plugin never reads the environment.
type Secret struct {
	Key         string `json:"key"`
	ConfigField string `json:"config_field"`
	Optional    bool   `json:"optional,omitempty"`
	DefaultEnv  string `json:"default_env,omitempty"`
	Doc         string `json:"doc,omitempty"`
}
