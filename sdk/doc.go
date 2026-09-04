// Package lore is the public contract every Lore plugin implements: the
// document shape a source normalizes to, the capability interfaces a plugin
// provides, and the values the host hands it at construction.
//
// It imports the standard library only. Nothing in it reaches into the engine,
// so a plugin compiled against this package needs no other Lore code.
package lore
