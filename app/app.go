// Package app assembles a lore binary from a set of plugins. It exists so that
// a third-party distribution is a composition root with one more argument
// rather than a fork: everything else about the engine is already independent
// of which plugins a build has.
package app

import (
	"net/http"
	"os"
	"time"

	"github.com/setthasit/Lore/internal/di"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/transport/cli"
	"github.com/setthasit/Lore/sdk"
)

// hostHTTPTimeout bounds one request a plugin makes through the shared client.
// It is generous because a plugin's own client may be stricter, and a source
// paginating a large project is legitimately slow.
const hostHTTPTimeout = 2 * time.Minute

// Option configures the binary being assembled.
type Option func(*options)

type options struct {
	plugins []lore.Plugin
}

// With registers plugins. It is variadic over the SDK interface, so an official
// set and a third-party plugin are passed the same way:
//
//	app.Run(app.With(append(plugins.Official(), acmecrm.Plugin())...))
func With(plugins ...lore.Plugin) Option {
	return func(o *options) { o.plugins = append(o.plugins, plugins...) }
}

// Run registers the plugins, then runs the command line over them. It returns
// the process exit code rather than exiting, so the whole binary stays testable.
func Run(opts ...Option) int {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	reg := registry.New(Host())
	if err := reg.Register(o.plugins...); err != nil {
		return cli.Report(os.Stderr, err)
	}
	return cli.Main(reg)
}

// Host is what every plugin this binary runs is lent. The client carries no
// retry policy of its own: retrying and honouring Retry-After is sdk/httpx's
// job, and layering a second retry underneath it would multiply the attempt
// budget instead of bounding it.
func Host() lore.Host {
	return lore.Host{
		HTTP: &http.Client{Timeout: hostHTTPTimeout},
		Log:  di.DiagnosticLogger(),
		Now:  time.Now,
	}
}
