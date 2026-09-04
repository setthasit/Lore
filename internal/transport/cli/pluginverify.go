package cli

import (
	"io"

	"context"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugexec"
	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
	"iter"
)

// certify runs the plugin certification suite against an installed binary. It
// is the same suite the official plugins run under `go test`: `sdk/conform`
// needs only a lore.Connector, and the exec client is one, so it neither knows
// nor cares that this implementation is a subprocess. That is the whole point —
// one suite certifies compiled and external plugins identically, and a
// third-party author runs this exact code path locally before publishing.
//
// The fixture is deliberately zero: a host verifying a stranger's binary cannot
// know how many documents it should yield or where a resume ought to start, so
// the suite asserts only the invariants derivable from the stream itself.
func certify(out io.Writer, name, binary string, host lore.Host) error {
	plugin, err := plugexec.Open(binary, host)
	if err != nil {
		return err
	}

	source, ok := plugin.(lore.SourcePlugin)
	if !ok {
		printfln(out, "  conformance: not run — %s is a %s plugin, and the suite certifies sources",
			name, plugin.Manifest().Kind)
		return nil
	}

	// A source is built with an empty configuration and no secrets: the suite
	// reads a stream, and a plugin that needs configuration to produce one says
	// so here rather than failing a check it never reached.
	newConnector := func() lore.Connector {
		conn, err := source.NewSource(lore.SourceConfig{Instance: name, Host: host})
		if err != nil {
			return failedConnector{name: name, err: err}
		}
		return conn
	}

	findings := conform.Check(newConnector, conform.Fixture{})
	if len(findings) == 0 {
		printfln(out, "  conformance: passed")
		return nil
	}

	printfln(out, "  conformance: %s", pluralize(len(findings), "failure", "failures"))
	for _, f := range findings {
		printfln(out, "    %s: %s", f.Check, f.Detail)
	}
	return internalerror.NewPreconditionError(
		name+" does not satisfy the plugin contract; the failures above name what a sync round would get wrong", nil)
}

// failedConnector turns a construction failure into a stream error, so the
// suite reports it as the finding it is instead of the command panicking on a
// nil connector.
type failedConnector struct {
	name string
	err  error
}

func (c failedConnector) Name() string { return c.name }

func (c failedConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) { yield(lore.Batch{}, c.err) }
}
