// Command muro is the thin CLI client over murod's control API
// (DESIGN.md §5/§9).
package main

import (
	"os"

	"github.com/thomkin/muro/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
