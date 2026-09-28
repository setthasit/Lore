// Package wire is the NDJSON protocol an out-of-process plugin exchanges with the host.
package wire

import (
	"encoding/json"
	"slices"

	"github.com/setthasit/Lore/sdk"
)

const (
	OpManifest = "manifest"
	OpChanges  = "changes"
	OpEmbed    = "embed"
	OpComplete = "complete"
	OpBlame    = "blame"
	OpLog      = "log"
	OpHasFile  = "has_file"
	OpRemote   = "matches_remote"
	OpShutdown = "shutdown"
)

// A frame longer than MaxLineBytes fails the operation; the cap is the
// protocol's, in docs/v3/09-plugin-protocol.md.
const MaxLineBytes = 8 << 20

type Envelope struct {
	V  int    `json:"v"`
	ID string `json:"id"`
	Op string `json:"op"`
}

type ManifestRequest struct {
	Envelope
}

type ShutdownRequest struct {
	Envelope
}

type ChangesRequest struct {
	Envelope
	Instance string            `json:"instance"`
	Config   json.RawMessage   `json:"config"`
	Secrets  map[string]string `json:"secrets"`
	Cursor   lore.Cursor       `json:"cursor"`
}

func (r ChangesRequest) MarshalJSON() ([]byte, error) {
	type wire ChangesRequest
	r.Secrets, r.Cursor = mapOrEmpty(r.Secrets), mapOrEmpty(r.Cursor)
	return json.Marshal(wire(r))
}

type EmbedRequest struct {
	Envelope
	Config  json.RawMessage   `json:"config"`
	Secrets map[string]string `json:"secrets"`
	Model   string            `json:"model"`
	Texts   []string          `json:"texts"`
}

func (r EmbedRequest) MarshalJSON() ([]byte, error) {
	type wire EmbedRequest
	r.Secrets, r.Texts = mapOrEmpty(r.Secrets), listOrEmpty(r.Texts)
	return json.Marshal(wire(r))
}

type CompleteRequest struct {
	Envelope
	Config  json.RawMessage   `json:"config"`
	Secrets map[string]string `json:"secrets"`
	Model   string            `json:"model"`
	System  string            `json:"system"`
	User    string            `json:"user"`
}

func (r CompleteRequest) MarshalJSON() ([]byte, error) {
	type wire CompleteRequest
	r.Secrets = mapOrEmpty(r.Secrets)
	return json.Marshal(wire(r))
}

type BlameRequest struct {
	Envelope
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// PathRequest carries OpLog and OpHasFile.
type PathRequest struct {
	Envelope
	Path string `json:"path"`
}

type RemoteRequest struct {
	Envelope
	Instance string            `json:"instance"`
	Config   json.RawMessage   `json:"config"`
	Secrets  map[string]string `json:"secrets"`
	Remote   string            `json:"remote"`
}

func (r RemoteRequest) MarshalJSON() ([]byte, error) {
	type wire RemoteRequest
	r.Secrets = mapOrEmpty(r.Secrets)
	return json.Marshal(wire(r))
}

type Frame struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Done  bool   `json:"done"`
	Error *Error `json:"error"`

	Manifest *lore.Manifest `json:"manifest"`
	Batch    *Batch         `json:"batch"`

	Vectors    [][]float32 `json:"vectors"`
	Dimensions int         `json:"dimensions"`
	Text       string      `json:"text"`

	Spans   []lore.BlameSpan `json:"spans"`
	Commits []lore.CommitRef `json:"commits"`
	Present bool             `json:"present"`
	Matches bool             `json:"matches"`
}

func (f Frame) MarshalJSON() ([]byte, error) {
	type wire Frame
	f.Vectors, f.Spans, f.Commits = rowsOrEmpty(f.Vectors), listOrEmpty(f.Spans), listOrEmpty(f.Commits)
	return json.Marshal(wire(f))
}

type Batch struct {
	Docs   []lore.Document `json:"docs"`
	Cursor *lore.Cursor    `json:"cursor"`
}

func (b Batch) MarshalJSON() ([]byte, error) {
	type wire Batch
	b.Docs = listOrEmpty(b.Docs)
	if b.Cursor == nil || *b.Cursor == nil {
		b.Cursor = &lore.Cursor{}
	}
	return json.Marshal(wire(b))
}

func listOrEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

func mapOrEmpty[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return M{}
	}
	return m
}

func rowsOrEmpty(rows [][]float32) [][]float32 {
	if !slices.ContainsFunc(rows, func(row []float32) bool { return row == nil }) {
		return listOrEmpty(rows)
	}

	out := make([][]float32, len(rows))
	for i, row := range rows {
		out[i] = listOrEmpty(row)
	}
	return out
}

type Error struct {
	Message string `json:"message"`
	Kind    string `json:"kind"`
}

// The kinds the host recognises; it reads any other kind as KindInternal and
// says so in the message it reports.
const (
	KindInvalidConfig = "invalid_config"
	KindAuth          = "auth"
	KindRateLimit     = "rate_limit"
	KindNotFound      = "not_found"
	KindInternal      = "internal"
)
