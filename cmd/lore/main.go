// Command lore is the composition root: the only file in the repository that
// names a plugin. A distribution with extra plugins compiled in is this file
// with more arguments to app.With.
package main

import (
	"os"

	"github.com/setthasit/Lore/app"
	"github.com/setthasit/Lore/plugins"
)

func main() {
	os.Exit(app.Run(app.With(plugins.Official()...)))
}
