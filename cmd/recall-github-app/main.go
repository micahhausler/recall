// Command recall-github-app is a recall credential command that mints a
// GitHub App installation token per repository. recall runs
// `recall-github-app token`; the other subcommands set the App up.
package main

import (
	"os"

	"github.com/micahhausler/recall/internal/githubapp/cli"
)

func main() {
	os.Exit(cli.Main())
}
