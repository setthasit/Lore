package plugexec

import (
	"encoding/json"

	"github.com/setthasit/Lore/sdk"
)

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

// A frame longer than maxLineBytes fails the operation; the cap is the
// protocol's, in docs/v3/09-plugin-protocol.md.
const maxLineBytes = 8 << 20

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

type wireBatch struct {
	Docs   []lore.Document `json:"docs"`
	Cursor *lore.Cursor    `json:"cursor"`
}

type wireError struct {
	Message string `json:"message"`
	Kind    string `json:"kind"`
}

// json.RawMessage(nil) marshals as `null`, which a plugin's config decoder never has to handle.
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
