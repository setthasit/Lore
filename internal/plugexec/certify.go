package plugexec

import (
	"context"
	"iter"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

// Certification is what the suite could say about one binary: a Kind other than
// KindSource means it never ran, because the suite certifies sources.
type Certification struct {
	Kind     lore.Kind
	Findings []conform.Finding
}

// The fixture is zero because a host cannot know a stranger's stream shape.
func Certify(instance, binary string, host lore.Host) (Certification, error) {
	plugin, err := Open(binary, host)
	if err != nil {
		return Certification{}, err
	}

	source, ok := plugin.(lore.SourcePlugin)
	if !ok {
		return Certification{Kind: plugin.Manifest().Kind}, nil
	}

	newConnector := func() lore.Connector {
		conn, err := source.NewSource(lore.SourceConfig{Instance: instance, Host: host})
		if err != nil {
			return failedConnector{name: instance, err: err}
		}
		return conn
	}
	return Certification{
		Kind:     lore.KindSource,
		Findings: conform.Check(newConnector, conform.Fixture{}),
	}, nil
}

type failedConnector struct {
	name string
	err  error
}

func (c failedConnector) Name() string { return c.name }

func (c failedConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) { yield(lore.Batch{}, c.err) }
}
