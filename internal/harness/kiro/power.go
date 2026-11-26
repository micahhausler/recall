package kiro

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/micahhausler/recall/internal/hook"
)

// PowerSkills lists the skills a power ships, the directories under its
// skills/.
func PowerSkills(power fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(power, "skills")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// InstallPower runs `<bin> powers install` on a copy of power in a
// fresh temp dir, which it removes afterward, and returns the CLI's last
// output line. The CLI installs only from a directory: no URL, file, or
// stdin. Installing over an installed power updates it.
func InstallPower(ctx context.Context, bin string, power fs.FS) (string, error) {
	tmp, err := os.MkdirTemp("", "recall-power-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	// The power's own directory is named like it, in case the CLI names
	// the install after the directory rather than plugin.json.
	dir := filepath.Join(tmp, "recall")
	if err := os.CopyFS(dir, power); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, "powers", "install", dir)
	cmd.Dir = tmp
	// The same guard model calls carry (model.HookGuard).
	cmd.Env = append(os.Environ(), hook.Guard)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String() + "\n" + out.String()); msg != "" {
			return "", fmt.Errorf("%s powers install: %w: %s", bin, err, msg)
		}
		return "", fmt.Errorf("%s powers install: %w", bin, err)
	}
	msg := strings.TrimSpace(out.String())
	if msg == "" {
		msg = strings.TrimSpace(errOut.String())
	}
	return msg[strings.LastIndex(msg, "\n")+1:], nil
}
