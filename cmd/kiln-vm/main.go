package main

import (
	"os"

	"github.com/gfdelarue/kiln-vm/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
