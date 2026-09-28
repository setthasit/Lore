package lore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
)

type Host struct {
	Log *slog.Logger // never nil
}

type SourceConfig struct {
	Instance string
	Config   json.RawMessage // the `with:` block, minus the keys that name secrets
	Secrets  map[string]string
	Host     Host
}

// Decode unmarshals the `with:` block strictly: an unknown key is an error,
// never a silently ignored typo.
func (c SourceConfig) Decode(v any) error { return decodeStrict(c.Instance, c.Config, v) }

// An undeclared key is empty.
func (c SourceConfig) Secret(key string) string { return c.Secrets[key] }

func (c SourceConfig) DocID(t DocType, external string) DocID {
	return NewDocID(c.Instance, t, external)
}

type ProviderConfig struct {
	Instance   string
	Capability Capability // a provider serving several builds only this one
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

type CodeConfig struct {
	Root string // workspace-absolute
	Host Host
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
