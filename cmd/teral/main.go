// Command teral is the Teral CLI.
package main

import (
	"context"
	"os"

	"github.com/teraldev/cli/internal/cmd"
)

func main() {
	os.Exit(cmd.Execute(context.Background()))
}
