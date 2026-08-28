// Package conformance is the shared contract suite every connector passes.
//
// The connector interface makes four promises the sync orchestrator relies on,
// and [Run] asserts all four against a connector wired to a fixed test source:
//
//	batch-cursor honesty  every batch carries the cursor to persist after it
//	document identity     every document is identified, citable and timestamped
//	idempotency           an unchanged source yields the same stream twice
//	resumability          resuming from a batch cursor loses nothing
//
// The suite depends on the standard library, [lore/internal/entities] and
// nothing else — the same dependency budget a connector has — so it can be
// handed to a third-party connector author verbatim as the certification suite.
// It drives the connector only through [entities.Connector]: how the source is
// stubbed, whether over httptest or recorded fixtures, is the caller's business.
// Everything the suite needs to know about that source is declared in a
// [Fixture].
package conformance
