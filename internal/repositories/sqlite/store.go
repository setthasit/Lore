// Package sqlite implements the workspace index on a single SQLite file.
//
// The driver is ncruces/go-sqlite3 (SQLite compiled to WASM, no cgo). The blank
// import of the sqlite-vec ncruces bindings is load-bearing: it supplies the
// WASM build that ncruces/go-sqlite3 runs, and that build has the sqlite-vec
// extension compiled in, which is what makes the vec0 virtual table available.
// It replaces the plain ncruces/go-sqlite3/embed binary; importing both would be
// a conflict.
//
// That prebuilt binary is also what pins the driver version; see the runtime
// configuration below.
//
// Errors are returned raw with context wrapping. Classifying them into
// internalerror kinds is the service layer's job.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"net/url"
	"time"

	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces" // SQLite WASM binary with sqlite-vec built in
	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"lore/internal/repositories"
)

var _ repositories.IndexStore = (*Store)(nil)

// The sqlite-vec WASM build uses atomics, which are outside the WASM core
// feature set the driver enables by default; without the threads feature the
// binary fails to compile at the first connection. Reaching this knob at all is
// why wazero is a direct dependency. The runtime configuration is process-wide
// and read once, when the first connection compiles the binary, so it is set
// here rather than per Open. Memory limit matches the driver's own default.
//
// The same prebuilt binary pins github.com/ncruces/go-sqlite3 to v0.23.1: it
// calls host functions that v0.24 renamed, and v0.33 removed the injectable
// binary entirely. An upgrade past v0.23.1 breaks at the first connection, not
// at compile time, until sqlite-vec ships a newer WASM build.
func init() {
	pages := uint32(4096) // 256 MiB
	if bits.UintSize < 64 {
		pages = 512 // 32 MiB
	}
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().
		WithMemoryLimitPages(pages).
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads)
}

// timeLayout is how the store writes timestamps: RFC 3339 UTC, second
// precision. Fixed width keeps lexicographic order chronological, so SQL
// time-range filters are plain string comparisons.
const timeLayout = time.RFC3339

// Store is the SQLite-backed index for one workspace file.
type Store struct {
	db         *sql.DB
	vectorDims int

	// now is the store's clock, injectable so lease TTL behaviour is testable.
	now func() time.Time
}

// Open opens (creating if absent) the workspace database at path and brings its
// schema up to date.
//
// path must already be resolved: no "~" expansion happens here, because the
// store has no opinion about whose home directory that is. Callers pass an
// absolute or working-directory-relative filesystem path.
//
// vectorDims is the embedder's vector width. It sizes the chunk_vectors table on
// a fresh file and is recorded in meta; reopening a file with a different width
// fails rather than silently mis-indexing, because the vec0 table keeps the
// dimensions it was created with.
func Open(path string, vectorDims int) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite: database path is empty")
	}
	if vectorDims <= 0 {
		return nil, fmt.Errorf("sqlite: vector dimensions must be positive, got %d", vectorDims)
	}

	db, err := driver.Open(dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}

	s := &Store{db: db, vectorDims: vectorDims, now: time.Now}
	if err := s.bootstrap(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: %s: %w", path, err)
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}
	return nil
}

// dsn builds the driver's file URI. Pragmas are applied to every pooled
// connection in the order given, and the driver only installs its own default
// busy timeout when none are specified, so busy_timeout comes first.
//
// WAL keeps readers off the writer's back for the daemon-plus-CLI case; the
// immediate transaction lock takes the write lock up front, turning a would-be
// mid-transaction upgrade conflict into a plain busy-wait.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(wal)")
	q.Add("_pragma", "foreign_keys(on)")
	q.Set("_txlock", "immediate")

	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}
	return u.String()
}
