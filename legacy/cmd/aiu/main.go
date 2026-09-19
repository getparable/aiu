// Command aiu shows usage and reset times for several Claude Code and Codex accounts.
// AIU.app, the menu bar panel, is a SwiftUI front end that runs this binary.
package main

import (
	"os"

	"github.com/getparable/aiu/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
