package plugexec

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/wire"
)

type call struct {
	external
	instance string
	config   json.RawMessage
	secrets  map[string]string
	model    string
}

type embedder struct {
	call
	dims int
}

type completer struct {
	call
}

var (
	_ lore.Embedder  = (*embedder)(nil)
	_ lore.Completer = (*completer)(nil)
)

func newEmbedder(c call, declared int) (*embedder, error) {
	if declared <= 0 {
		return nil, protocolError(c.instance, wire.OpEmbed, nil,
			"embedder.dimensions must be set: the protocol reports a width only in an embed response, "+
				"and the index's vector column is created before the first document is embedded")
	}
	return &embedder{call: c, dims: declared}, nil
}

func (e *embedder) Dimensions() int { return e.dims }

func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	frame, err := e.unary(ctx, e.instance, wire.OpEmbed, e.tuning.unary, func(env wire.Envelope) any {
		return wire.EmbedRequest{Envelope: env, Config: e.config, Secrets: e.secrets, Model: e.model, Texts: texts}
	})
	if err != nil {
		return nil, err
	}
	return e.aligned(frame, texts)
}

func (e *embedder) aligned(frame *wire.Frame, texts []string) ([][]float32, error) {
	if len(frame.Vectors) != len(texts) {
		return nil, protocolError(e.instance, wire.OpEmbed, nil,
			"answered %d texts with %d vectors; a short, reordered or filtered result is not a partial success",
			len(texts), len(frame.Vectors))
	}
	if frame.Dimensions <= 0 {
		return nil, protocolError(e.instance, wire.OpEmbed, nil, "reported dimensions %d, which is not a vector width", frame.Dimensions)
	}
	// Vectors already stored in this instance's index cannot be reinterpreted at a new width.
	if frame.Dimensions != e.dims {
		return nil, protocolError(e.instance, wire.OpEmbed, nil,
			"reported dimensions %d, but this instance's vector space is %d wide", frame.Dimensions, e.dims)
	}
	for i, vector := range frame.Vectors {
		if len(vector) != frame.Dimensions {
			return nil, protocolError(e.instance, wire.OpEmbed, nil,
				"vectors[%d] holds %d values, but the response reports %d dimensions", i, len(vector), frame.Dimensions)
		}
	}
	return frame.Vectors, nil
}

func (c *completer) Complete(ctx context.Context, system, user string) (string, error) {
	frame, err := c.unary(ctx, c.instance, wire.OpComplete, c.tuning.complete, func(env wire.Envelope) any {
		return wire.CompleteRequest{
			Envelope: env,
			Config:   c.config,
			Secrets:  c.secrets,
			Model:    c.model,
			System:   system,
			User:     user,
		}
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(frame.Text) == "" {
		adjective := "whitespace-only"
		if frame.Text == "" {
			adjective = "empty"
		}
		return "", protocolError(c.instance, wire.OpComplete, nil, "answered complete with %s text", adjective)
	}
	return frame.Text, nil
}
