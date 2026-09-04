package plugexec

import (
	"encoding/json"

	"github.com/setthasit/Lore/sdk"
)

// The operations of the protocol. They are strings on the wire and constants
// here because every error message names the op that failed.
const (
	opManifest = "manifest"
	opChanges  = "changes"
	opEmbed    = "embed"
	opComplete = "complete"
	opBlame    = "blame"
	opLog      = "log"
	opHasFile  = "has_file"
	opRemote   = "matches_remote"
	opShutdown = "shutdown"
)

// maxLineBytes caps one NDJSON frame. A frame longer than this fails the
// operation rather than growing the host's heap on a plugin's say-so: a batch
// too large to frame has to be split into several batch frames, which is always
// legal because the batch is the checkpoint unit.
const maxLineBytes = 8 << 20

// envelope is the part of every request the plugin echoes back. It is embedded
// rather than repeated so no op can forget a field, and JSON flattens embedded
// structs, so the wire form is one flat object exactly as the protocol shows it.
type envelope struct {
	V  int    `json:"v"`
	ID string `json:"id"`
	Op string `json:"op"`
}

type manifestRequest struct {
	envelope
}

type shutdownRequest struct {
	envelope
}

// The payload keys are present even when empty — `"config": {}`, `"secrets": {}`
// — so a plugin author can decode a request without treating an absent key and
// an empty one as different cases.
type changesRequest struct {
	envelope
	Instance string            `json:"instance"`
	Config   json.RawMessage   `json:"config"`
	Secrets  map[string]string `json:"secrets"`
	Cursor   lore.Cursor       `json:"cursor"`
}

type embedRequest struct {
	envelope
	Config  json.RawMessage   `json:"config"`
	Secrets map[string]string `json:"secrets"`
	Model   string            `json:"model"`
	Texts   []string          `json:"texts"`
}

type completeRequest struct {
	envelope
	Config  json.RawMessage   `json:"config"`
	Secrets map[string]string `json:"secrets"`
	Model   string            `json:"model"`
	System  string            `json:"system"`
	User    string            `json:"user"`
}

// A code request carries neither config nor secrets: the path is
// workspace-absolute because the host resolved it against the registered clone
// root, and a local clone needs no credentials.
type blameRequest struct {
	envelope
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type pathRequest struct {
	envelope
	Path string `json:"path"`
}

type remoteRequest struct {
	envelope
	Instance string            `json:"instance"`
	Config   json.RawMessage   `json:"config"`
	Secrets  map[string]string `json:"secrets"`
	Remote   string            `json:"remote"`
}

// frame is every response the protocol defines, in one type: a plugin answers
// one op at a time, so the fields a given op does not use are absent. Decoding
// with the standard library ignores unknown fields, which is what makes the
// protocol's additive evolution safe on this side.
type frame struct {
	V     int        `json:"v"`
	ID    string     `json:"id"`
	OK    bool       `json:"ok"`
	Done  bool       `json:"done"`
	Error *wireError `json:"error"`

	Manifest *lore.Manifest `json:"manifest"`
	Batch    *wireBatch     `json:"batch"`

	Vectors    [][]float32 `json:"vectors"`
	Dimensions int         `json:"dimensions"`
	Text       string      `json:"text"`

	Spans   []lore.BlameSpan `json:"spans"`
	Commits []lore.CommitRef `json:"commits"`
	Present bool             `json:"present"`
	Matches bool             `json:"matches"`
}

// wireBatch keeps Cursor a pointer so an absent cursor is distinguishable from
// a present one: a batch frame without a cursor checkpoints nothing and makes
// crash-safe resume unimplementable, so it is refused rather than committed.
type wireBatch struct {
	Docs   []lore.Document `json:"docs"`
	Cursor *lore.Cursor    `json:"cursor"`
}

type wireError struct {
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Kind      string `json:"kind"`
}

// emptyObject is what an absent config becomes on the wire. json.RawMessage(nil)
// would marshal as `null`, and a plugin decoding `null` into its config struct
// sees something it never has to handle for a compiled plugin.
func emptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func secretsOrEmpty(secrets map[string]string) map[string]string {
	if secrets == nil {
		return map[string]string{}
	}
	return secrets
}

func cursorOrEmpty(cursor lore.Cursor) lore.Cursor {
	if cursor == nil {
		return lore.Cursor{}
	}
	return cursor
}
