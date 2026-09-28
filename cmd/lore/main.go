package main

import (
	"os"

	"github.com/setthasit/Lore/app"
	"github.com/setthasit/Lore/plugins"
)

func main() {
	os.Exit(app.Run(plugins.Official()...))
}
