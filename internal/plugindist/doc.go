// Package plugindist is the supply chain for external plugins: what a `from:`
// coordinate means, how the binary behind it is fetched, how it is pinned in
// lore.lock, where it is cached, and what refuses to run.
//
// It is the only package that touches the supply chain, and it deliberately
// stops at the binary. It never speaks the plugin protocol and never performs
// the manifest handshake: it hands back a verified path, and the caller opens
// it. That keeps two things separable — where code came from, and what it says
// it can do — and it is why a manifest is always read from the binary rather
// than from a file inside an archive.
//
// The invariants, in the order they bite:
//
//   - A remote coordinate pins an exact version in lore.yaml. @latest is an
//     argument to `lore plugin install`, which writes back what it resolved.
//   - Nothing downloads implicitly. A declared-but-uninstalled plugin is a
//     startup error naming the command that fixes it, never a fetch.
//   - A digest mismatch refuses. Not at install, not at launch, and no flag
//     makes it continue.
//   - The lockfile decides which installed version runs. The cache may hold
//     several and never picks.
//
// The design of record is docs/v3/10-plugin-distribution.md.
package plugindist
