package learn

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The harness refuses a headless edit to any file under a directory named
// .claude, as a sensitive file, whatever the permission mode or allow rules
// say. The default targets (~/.claude/CLAUDE.md and its imports, skills
// under ~/.claude/skills) all sit under one, so the apply model edits
// copies instead: each .claude directory its input names is copied to a
// shadow directory in the run directory, the input points at the copy, and
// after the model is done the binary copies what changed back into the
// worktree. The model has no delete tool, so a file missing from the shadow
// afterward is left alone.

// shadowDir is the run directory's folder for shadow copies.
const shadowDir = "shadow"

// claudeDirRef matches a path that runs through a .claude directory.
var claudeDirRef = regexp.MustCompile(`(/[^\s` + "`" + `"']*?/\.claude)(?:/|\b)`)

// shadow is one .claude directory copied out of a worktree.
type shadow struct {
	real, copy string
}

// shadowClaudeDirs copies every .claude directory under one of roots that
// input names into runDir's shadow folder, and returns the input pointing
// at the copies, the shadow folder (empty when nothing was copied), and the
// copies to sync back.
func shadowClaudeDirs(input, runDir string, roots []string) (string, string, []shadow, error) {
	seen := map[string]bool{}
	var reals []string
	for _, m := range claudeDirRef.FindAllStringSubmatch(input, -1) {
		dir := filepath.Clean(m[1])
		if seen[dir] || !under(dir, roots) {
			continue
		}
		seen[dir] = true
		reals = append(reals, dir)
	}
	if len(reals) == 0 {
		return input, "", nil, nil
	}
	// Longest first, so a nested .claude is rewritten before its parent.
	sort.Slice(reals, func(i, j int) bool { return len(reals[i]) > len(reals[j]) })
	root := filepath.Join(runDir, shadowDir)
	var out []shadow
	for i, real := range reals {
		cp := filepath.Join(root, fmt.Sprint(i), "dot-claude")
		if err := copyTree(real, cp); err != nil {
			return "", "", nil, err
		}
		out = append(out, shadow{real: real, copy: cp})
		input = regexp.MustCompile(regexp.QuoteMeta(real)+`(/|\b|$)`).ReplaceAllString(input, cp+"$1")
	}
	return input, root, out, nil
}

// unshadow points the model's output back at the worktree paths, so its
// outcomes name the files the binary stages and commits.
func unshadow(out string, shadows []shadow) string {
	for _, s := range shadows {
		out = strings.ReplaceAll(out, s.copy, s.real)
	}
	return out
}

// syncBack copies every file the model changed or created in a shadow back
// to its place in the worktree. It never writes through a symlink: a
// symlinked file or directory under the .claude directory isn't copied out,
// so a file the model creates at that path would otherwise land wherever
// the link points, possibly outside the worktree. Only routed paths are
// staged afterward (Applier.land), so a change to any other file stays out
// of the commit.
func syncBack(shadows []shadow) error {
	for _, s := range shadows {
		err := filepath.WalkDir(s.copy, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(s.copy, p)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			dst := filepath.Join(s.real, rel)
			if err := noSymlinkBetween(s.real, dst); err != nil {
				return err
			}
			if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			return os.WriteFile(dst, data, 0o644)
		})
		if err != nil {
			return fmt.Errorf("copying edits back to %s: %w", s.real, err)
		}
	}
	return nil
}

// copyTree copies a directory's regular files; a missing src is an empty
// tree, for a .claude directory the model creates.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// noSymlinkBetween refuses a destination when root, or any part of the
// path from root down to dst that exists, is a symlink.
func noSymlinkBetween(root, dst string) error {
	rel, err := filepath.Rel(root, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside %s", dst, root)
	}
	p := root
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write %s: %s is a symlink", dst, p)
		}
	}
	return nil
}

func under(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
