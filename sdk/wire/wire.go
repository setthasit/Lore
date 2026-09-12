// Package wire is the NDJSON protocol an out-of-process plugin exchanges with the host.
package wire

import (
	"encoding/json"

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

type EmbedRequest struct {
	Envelope
	Config  json.RawMessage   `json:"config"`
	Secrets map[string]string `json:"secrets"`
	Model   string            `json:"model"`
	Texts   []string          `json:"texts"`
}

type CompleteRequest struct {
	Envelope
	Config  json.RawMessage   `json:"config"`
	Secrets map[string]string `json:"secrets"`
	Model   string            `json:"model"`
	System  string            `json:"system"`
	User    string            `json:"user"`
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

type Batch struct {
	Docs   []lore.Document `json:"docs"`
	Cursor *lore.Cursor    `json:"cursor"`
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
