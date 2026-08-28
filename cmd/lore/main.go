package main

import (
	"fmt"
	"os"

	"lore/internal/transport/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
