// Package plugexec runs an out-of-process plugin and presents it to the engine
// as the ordinary SDK values a compiled plugin presents: a lore.Connector, an
// Embedder, a Completer, a CodeRepo. It is the only package that speaks the
// wire protocol in docs/v3/09-plugin-protocol.md, so the services layer never
// learns which mode a plugin came from and a protocol change touches one place.
package plugexec
