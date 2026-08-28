// Package sqlite implements the workspace index on a single SQLite file.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"net/url"
	"time"

	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces" // WASM build with sqlite-vec compiled in; replaces go-sqlite3/embed, cannot coexist with it
	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"lore/internal/repositories"
)

var _ repositories.IndexStore = (*Store)(nil)

// The sqlite-vec WASM build uses atomics, so the driver needs the threads
// feature; without it the binary fails to compile at the first connection.
//
// That prebuilt binary pins github.com/ncruces/go-sqlite3 to v0.23.1: v0.24
// renamed host functions it calls and v0.33 removed the injectable binary, so a
// later version breaks at the first connection, not at compile time.
func init() {
	pages := uint32(4096) // 256 MiB
	if bits.UintSize < 64 {
		pages = 512 // 32 MiB
	}
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().
		WithMemoryLimitPages(pages).
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads)
}

// Fixed width keeps lexicographic order chronological, so SQL time-range
// filters are plain string comparisons.
const timeLayout = time.RFC3339

type Store struct {
	db         *sql.DB
	vectorDims int

	now func() time.Time
}

// path must already be resolved; no "~" expansion happens here. vectorDims
// sizes chunk_vectors on a fresh file and is then fixed: reopening with a
// different width fails, because the vec0 table keeps its original dimensions.
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

func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}
	return nil
}

// The driver installs its own default busy timeout only when no pragma sets
// one, so busy_timeout comes first.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(wal)")
	q.Add("_pragma", "foreign_keys(on)")
	q.Set("_txlock", "immediate")

	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}
	return u.String()
}
