// Package extension reads the owner's extension files: instructions that a
// recall skill or prompt appends at load time, so personal behavior lives in
// the owner's own files and never in this repo.
package extension

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Read returns the files' contents in order, each ending in a newline and
// separated by a blank line. A missing or unreadable file is reported to
// warn and skipped: an extension never stops the skill it extends.
func Read(paths []string, warn io.Writer) string {
	var parts []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			if warn != nil {
				fmt.Fprintf(warn, "recall: extension %s: %v (skipped)\n", p, err)
			}
			continue
		}
		text := strings.TrimRight(string(b), "\n")
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text+"\n")
		}
	}
	return strings.Join(parts, "\n")
}
