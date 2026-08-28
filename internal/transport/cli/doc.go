// Package cli implements the lore command-line surface.
//
// Every command reads the workspace configuration named by --config (default
// ./lore.yaml), resolves the service layer from it through the fx modules in
// internal/di, and calls services only — nothing here touches the index or a
// connector (02 — Layering).
//
// Failures go to stderr and set an exit code per internalerror kind: 1
// internal, 2 bad request, 3 precondition, 4 not found (see errors.go). Human
// output goes to stdout, so `lore ask --raw` is pipeable and `lore mcp` owns the
// stream it speaks its protocol on.
package cli
