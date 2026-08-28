// Package di wires the application graph as Uber FX modules.
//
// One module per layer, in the dependency order of 02-architecture.md:
// configuration, embedder, repository, connectors, services. Workspace bundles
// them for a transport, which asks the graph for services and nothing else.
//
// This is the only layer that reads the environment: connectors and the
// embedder take explicit values, so the *_env indirection in lore.yaml is
// resolved to real secrets exactly here. It is also where a configured path
// becomes a real one — "~" expansion and creating the index's directory — since
// both are facts about the machine rather than about SQLite.
package di
