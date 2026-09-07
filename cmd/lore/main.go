// Command lore is the composition root: the only file in the repository that
// names a plugin.
package main

import (
	"os"

	"github.com/setthasit/Lore/app"
	"github.com/setthasit/Lore/plugins"
)

func main() {
	os.Exit(app.Run(plugins.Official()...))
}
