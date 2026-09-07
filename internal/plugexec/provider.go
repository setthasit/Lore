package plugexec

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/setthasit/Lore/sdk"
)

// call is one configured provider instance: the payload every provider op
// repeats, held once so the two capabilities cannot drift apart.
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
		return nil, protocolError(c.instance, opEmbed, nil,
			"embedder.dimensions must be set: the protocol reports a width only in an embed response, "+
				"and the index's vector column is created before the first document is embedded")
	}
	return &embedder{call: c, dims: declared}, nil
}

func (e *embedder) Dimensions() int { return e.dims }

func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	// The SDK contract is explicit that no texts means no request, so an empty
	// caller batch never costs a process.
	if len(texts) == 0 {
		return nil, nil
	}

	frame, err := e.unary(ctx, e.instance, opEmbed, e.tuning.unary, func(env envelope) any {
		return embedRequest{envelope: env, Config: e.config, Secrets: e.secrets, Model: e.model, Texts: texts}
	})
	if err != nil {
		return nil, err
	}
	return e.aligned(frame, texts)
}

// Misalignment is never a partial success: the host would otherwise store one
// document's vector under another's id.
func (e *embedder) aligned(frame *frame, texts []string) ([][]float32, error) {
	if len(frame.Vectors) != len(texts) {
		return nil, protocolError(e.instance, opEmbed, nil,
			"answered %d texts with %d vectors; a short, reordered or filtered result is not a partial success",
			len(texts), len(frame.Vectors))
	}
	if frame.Dimensions <= 0 {
		return nil, protocolError(e.instance, opEmbed, nil, "reported dimensions %d, which is not a vector width", frame.Dimensions)
	}
	// A width that moves under a live index cannot be reinterpreted afterwards.
	if frame.Dimensions != e.dims {
		return nil, protocolError(e.instance, opEmbed, nil,
			"reported dimensions %d, but this instance's vector space is %d wide", frame.Dimensions, e.dims)
	}
	for i, vector := range frame.Vectors {
		if len(vector) != frame.Dimensions {
			return nil, protocolError(e.instance, opEmbed, nil,
				"vectors[%d] holds %d values, but the response reports %d dimensions", i, len(vector), frame.Dimensions)
		}
	}
	return frame.Vectors, nil
}

func (c *completer) Complete(ctx context.Context, system, user string) (string, error) {
	frame, err := c.unary(ctx, c.instance, opComplete, c.tuning.complete, func(env envelope) any {
		return completeRequest{
			envelope: env,
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
		return "", protocolError(c.instance, opComplete, nil, "answered complete with %s text", adjective)
	}
	return frame.Text, nil
}
