// Package config loads and validates lore.yaml workspace configuration.
//
// The model is plugin-driven: `sources:` and `providers:` are lists of
// instances that name a plugin with `use:` and configure it with an opaque
// `with:` block. Nothing here knows a source or a provider by name, so this
// package never changes when one is added. It owns the format-level rules only
// — an instance is addressable, its identity is unique, the paths it names
// exist. Everything that needs a plugin's manifest to answer, from the meaning
// of a `with:` key to whether a bound provider can embed, belongs to the
// registry that holds the manifests.
//
// Decode is the only strict YAML decoder in the repository, shared by Load and
// by the commands that read a configuration in order to rewrite it.
package config
