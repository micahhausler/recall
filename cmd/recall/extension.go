package main

import (
	"fmt"
	"os"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/extension"
)

const extensionUsage = `Usage:
  recall extension NAME

Prints the owner's extension files for NAME (a skill name, or
learn-reduce), from extensions.NAME in config, in order. Prints nothing when
none are set. A missing file is warned about on stderr and skipped; the
command always exits 0, so a skill that injects its output never aborts.
`

func cmdExtension(args []string) int {
	if len(args) != 1 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, extensionUsage)
		if len(args) == 1 {
			return 0
		}
		return 2
	}
	s, err := loadSettings()
	if err != nil {
		// Even a broken config mustn't abort the skill; say why and stop.
		fmt.Fprintf(os.Stderr, "recall: extension %s: %v (skipped)\n", args[0], err)
		return 0
	}
	fmt.Print(extension.Read(s.Extensions[args[0]], os.Stderr))
	return 0
}

// loadSettings reads the merged config without opening harnesses or the
// store, for commands that only need settings.
func loadSettings() (*config.Settings, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	c, err := config.Load(config.DefaultBase(home))
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	return c.Settings(home, shortHost(hostname))
}
