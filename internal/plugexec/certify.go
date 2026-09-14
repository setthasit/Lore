package plugexec

import (
	"context"
	"errors"
	"iter"

	"github.com/setthasit/Lore/sdk"
	"github.com/setthasit/Lore/sdk/conform"
)

// Certification is what the suite could say about one binary: a Kind other than
// KindSource means it never ran, because the suite certifies sources.
type Certification struct {
	Kind lore.Kind
	conform.Result
}

func Certify(instance, binary string, host lore.Host, config []byte, secrets map[string]string, declared bool) (Certification, error) {
	plugin, err := Open(binary, host)
	if err != nil {
		return Certification{}, err
	}

	source, ok := plugin.(lore.SourcePlugin)
	if !ok {
		return Certification{Kind: plugin.Manifest().Kind}, nil
	}

	newConnector := func() lore.Connector {
		conn, err := source.NewSource(lore.SourceConfig{
			Instance: instance,
			Config:   config,
			Secrets:  secrets,
			Host:     host,
		})
		if err != nil {
			return failedConnector{name: instance, err: err}
		}
		return conn
	}
	var unconfigured func(error) bool
	if !declared {
		unconfigured = refusesWithoutConfiguration
	}
	return Certification{
		Kind:   lore.KindSource,
		Result: conform.Check(newConnector, conform.Fixture{}, unconfigured),
	}, nil
}

func refusesWithoutConfiguration(err error) bool {
	var refused *pluginError
	if !errors.As(err, &refused) {
		return false
	}
	return refused.kind == kindInvalidConfig || refused.kind == kindAuth
}

type failedConnector struct {
	name string
	err  error
}

func (c failedConnector) Name() string { return c.name }

func (c failedConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) { yield(lore.Batch{}, c.err) }
}
