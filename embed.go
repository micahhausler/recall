// Package recall carries files from the repository root into the recall
// binary, which go:embed can only reach from a package at the root.
package recall

import (
	"embed"
	"io/fs"
)

// The power ships only keep and recall-setup; the other skills wrap one
// subcommand each and aren't worth a Kiro install.
//
//go:embed plugin.json skills/keep skills/recall-setup
var kiroPower embed.FS

// KiroPower is the Kiro power `recall init` installs: plugin.json, the
// Agent Plugins manifest, and skills/<name>/ for each skill it ships.
func KiroPower() fs.FS { return kiroPower }
