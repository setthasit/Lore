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

// probeText is embedded once when the operator declared no width. Its content
// is irrelevant: only the width of the vector that comes back is read.
const probeText = "lore dimension probe"

func newEmbedder(c call, declared int) (*embedder, error) {
	e := &embedder{call: c, dims: declared}
	if declared > 0 {
		return e, nil
	}

	// lore.Embedder answers Dimensions() synchronously because the index's
	// vector column is created before the first document is embedded, while the
	// protocol reports dimensions only in an embed response. So a width nobody
	// declared is learned by embedding once here, at construction, where a
	// failure is a configuration error rather than a half-written index.
	frame, err := c.unary(context.Background(), c.instance, opEmbed, c.tuning.unary, func(env envelope) any {
		return embedRequest{envelope: env, Config: c.config, Secrets: c.secrets, Model: c.model, Texts: []string{probeText}}
	})
	if err != nil {
		return nil, err
	}
	vectors, err := e.aligned(frame, []string{probeText})
	if err != nil {
		return nil, err
	}
	e.dims = len(vectors[0])
	return e, nil
}

// Dimensions is the width every vector this provider returns carries; the host
// composes the vector-space identity from it, so the plugin never names itself.
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

// aligned enforces the one rule that makes an embedding response usable: the
// vectors are positionally aligned with the texts. A short, reordered or
// filtered result is a protocol error and never a partial success, because the
// host would otherwise store one document's vector under another's id.
func (e *embedder) aligned(frame *frame, texts []string) ([][]float32, error) {
	if len(frame.Vectors) != len(texts) {
		return nil, protocolError(e.instance, opEmbed,
			"answered %d texts with %d vectors; a short, reordered or filtered result is not a partial success",
			len(texts), len(frame.Vectors))
	}
	if frame.Dimensions <= 0 {
		return nil, protocolError(e.instance, opEmbed, "reported dimensions %d, which is not a vector width", frame.Dimensions)
	}
	// A width that moves under a live index cannot be reinterpreted afterwards,
	// so a provider that changes its mind fails the operation instead.
	if e.dims > 0 && frame.Dimensions != e.dims {
		return nil, protocolError(e.instance, opEmbed,
			"reported dimensions %d, but this instance's vector space is %d wide", frame.Dimensions, e.dims)
	}
	for i, vector := range frame.Vectors {
		if len(vector) != frame.Dimensions {
			return nil, protocolError(e.instance, opEmbed,
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
	// An empty completion is indistinguishable from a dropped request, so it is
	// an error rather than an answer nobody can cite.
	if strings.TrimSpace(frame.Text) == "" {
		return "", protocolError(c.instance, opComplete, "answered complete with %s text", emptiness(frame.Text))
	}
	return frame.Text, nil
}

func emptiness(text string) string {
	if text == "" {
		return "empty"
	}
	return "whitespace-only"
}
