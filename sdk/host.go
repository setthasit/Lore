package lore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Host is what the engine lends a plugin: a retrying HTTP client, a logger, and
// a clock. Everything else a plugin needs arrives as configuration, because a
// plugin that could reach further could reach the store.
type Host struct {
	// HTTP retries what a server reports as temporary and honours Retry-After.
	HTTP *http.Client

	// Log is already tagged with the instance id.
	Log *slog.Logger

	// Now is injectable so a plugin's time-dependent behavior stays testable.
	Now func() time.Time
}

// SourceConfig is everything a source instance is built from. Instance is the
// identity the whole engine keys on, so a connector never spells its own name.
type SourceConfig struct {
	Instance string          // "jira-acme" — cursor key, Source value, DocID prefix
	Config   json.RawMessage // the `with:` block, minus the keys that name secrets
	Secrets  map[string]string
	Host     Host
}

// Decode unmarshals the `with:` block strictly: a key the plugin does not know
// is an error, never a silently ignored typo.
func (c SourceConfig) Decode(v any) error { return decodeStrict(c.Instance, c.Config, v) }

// Secret returns the resolved value of a secret the manifest declared. An
// undeclared key is empty: needing a value the manifest never asked for means
// the plugin is misconfigured, not that the host withheld something.
func (c SourceConfig) Secret(key string) string { return c.Secrets[key] }

// DocID builds a document identity prefixed with this instance's id, which is
// what keeps two instances of one plugin out of each other's namespace.
func (c SourceConfig) DocID(t DocType, external string) DocID {
	return NewDocID(c.Instance, t, external)
}

// ProviderConfig is everything a provider instance is built from for one role
// binding. Capability says which half of a multi-capability provider to build;
// Model and Dimensions come from the binding, not the instance, because one
// connection serves several models.
type ProviderConfig struct {
	Instance   string
	Capability Capability
	Model      string

	// Dimensions is the vector width the operator declared, for drivers whose
	// models do not imply one. Zero means the driver decides.
	Dimensions int

	Config  json.RawMessage
	Secrets map[string]string
	Host    Host
}

func (c ProviderConfig) Decode(v any) error { return decodeStrict(c.Instance, c.Config, v) }

func (c ProviderConfig) Secret(key string) string { return c.Secrets[key] }

// CodeConfig binds a code plugin to one clone. Root is workspace-absolute — the
// host resolves it before construction — and there are no secrets, because a
// local clone needs no credentials.
type CodeConfig struct {
	Root   string
	Remote string // "github:acme/app"; empty when the clone maps onto no source
	Host   Host
}

func decodeStrict(instance string, raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("%s: invalid configuration: %w", instance, err)
	}
	return nil
}
