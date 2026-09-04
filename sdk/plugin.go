package lore

// APIVersion is the contract version this SDK implements. A plugin declares the
// version it was built against and the host refuses a mismatch, naming both
// numbers: running a source over a contract the two sides do not agree on
// corrupts an index in ways no later validation can find.
const APIVersion = 1

// Kind is what a plugin extends. Each kind has exactly one construction method
// and exactly one capability interface it must produce.
type Kind string

const (
	KindSource   Kind = "source"
	KindProvider Kind = "provider"
	KindCode     Kind = "code"
)

// Plugin is the common half of every plugin: it describes itself before it is
// asked to build anything, so a misconfiguration is caught before a sync round.
type Plugin interface {
	Manifest() Manifest
}

// SourcePlugin builds connectors. One plugin serves any number of instances:
// two Jira sites are two SourceConfigs, not two plugins.
type SourcePlugin interface {
	Plugin
	NewSource(SourceConfig) (Connector, error)
}

// ProviderPlugin builds model providers. The returned Provider is asserted
// against the manifest's Capabilities by the host, which rejects a mismatch.
type ProviderPlugin interface {
	Plugin
	NewProvider(ProviderConfig) (Provider, error)
}

// CodePlugin builds read-only accessors for one local clone.
type CodePlugin interface {
	Plugin
	NewCode(CodeConfig) (CodeRepo, error)
}

// Provider is deliberately unconstrained: an embedding provider and a
// completion provider are the same kind of plugin differing only in the
// optional interfaces they satisfy. The host asserts them against the manifest.
type Provider = any

// RemoteMatcher is the optional interface a Connector implements when its
// manifest declares Capabilities.RepoRemotes. It answers whether a registered
// local clone's "<forge>:<namespace>/<name>" remote is one this instance
// ingests, which is what keeps the startup warning about an unmatched clone
// working without the engine knowing a forge by name.
type RemoteMatcher interface {
	MatchesRemote(remote string) bool
}

// Manifest is the single description of a plugin's configuration. It is not
// documentation: the host generates scaffolds, prompts, validation and error
// text from it, so a field that is not declared here does not exist.
type Manifest struct {
	Name         string       `json:"name"` // "github", "openai-compatible", "git"
	Kind         Kind         `json:"kind"`
	APIVersion   int          `json:"api_version"` // must equal APIVersion
	Summary      string       `json:"summary"`     // one line; shown by `lore plugin list`
	Capabilities Capabilities `json:"capabilities"`
	Fields       []Field      `json:"fields"` // the plugin's `with:` block
	Secrets      []Secret     `json:"secrets"`

	// DefaultModels suggests one model per capability, for scaffolds and prompts.
	// The host never applies a suggestion: a role binding always names its model,
	// because a silently defaulted embedding model is a silently defaulted vector
	// space, and the index it built cannot be reinterpreted afterwards.
	DefaultModels map[Capability]string `json:"default_models,omitempty"`
}

// Capabilities are the optional behaviors a manifest claims. The host verifies
// each claim against the built value and refuses a role binding to a capability
// the plugin did not declare.
type Capabilities struct {
	Embed       bool `json:"embed"`        // provider serves embeddings
	Complete    bool `json:"complete"`     // provider serves completions
	RepoRemotes bool `json:"repo_remotes"` // source documents carry repo paths a local clone maps onto
}

// Capability is the role a provider is being built for. It is part of
// ProviderConfig because a chat model and an embedding model are configured
// differently: a provider that serves both builds only the half it was asked
// for, and the host then asserts the built value against that half.
type Capability string

const (
	CapabilityEmbed    Capability = "embed"
	CapabilityComplete Capability = "complete"
)

// Declares reports whether these Capabilities include c.
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

// FieldType is the shape a configuration value must have. The host checks it
// before the plugin's own decoder runs, so a type error names the field.
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
	Name     string    `json:"name"` // "base_url"
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`

	// Default documents the value the plugin falls back to. The host shows it in
	// scaffolds and prompts but never injects it: applying it belongs to the
	// plugin, which is the only side that knows the value's real type.
	Default string `json:"default,omitempty"`

	Doc    string `json:"doc,omitempty"`    // shown in `lore init` scaffolds and errors
	Prompt string `json:"prompt,omitempty"` // question `lore source add` asks
}

// Secret is a credential a plugin needs. The plugin names the key it wants;
// the operator names the environment variable holding it; the host resolves the
// variable and injects the value. A plugin never reads the environment, which
// is what makes a per-plugin secret allowlist possible and what stops one
// plugin from reading another's token.
type Secret struct {
	Key         string `json:"key"`          // "token" — how the plugin asks for it
	ConfigField string `json:"config_field"` // "token_env" — the config key naming the env var
	DefaultEnv  string `json:"default_env,omitempty"`
	Doc         string `json:"doc,omitempty"`
}
