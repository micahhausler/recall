package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/micahhausler/recall/internal/redact"
)

// zstdWindow is 2^27: the long-distance window finds repeats across the many
// similar JSONL lines, and 2^27 is within zstd's default decompression limit,
// so plain `zstd -d` reads the output without flags.
const zstdWindow = 1 << 27

// Options change what a week's tarball holds.
type Options struct {
	// Redact replaces secret-shaped tokens in text members (archive-chats.redact).
	Redact bool
}

// Built is what a build archived.
type Built struct {
	// Files are the members, relative to the base.
	Files []string
	// Redactions is how many tokens Options.Redact replaced.
	Redactions int
}

// redactable are the members Options.Redact rewrites: transcripts and the
// text files a harness keeps beside them. Anything else is archived as is.
var redactable = map[string]bool{".jsonl": true, ".json": true, ".txt": true, ".md": true}

// Build tars the files (relative to base) and zstd-compresses them to out.
// Compression matches `zstd -3 --long=27` closely enough to keep the archive
// layout readable by plain zstd. Files deleted since selection (a harness's
// own cleanup can race a run) are dropped rather than failing; the files
// actually archived are returned, and nothing is written when every file
// vanished.
func Build(base string, files []string, out io.Writer, opts Options) (Built, error) {
	var b Built
	for _, f := range files {
		if st, err := os.Lstat(filepath.Join(base, filepath.FromSlash(f))); err == nil && st.Mode().IsRegular() {
			b.Files = append(b.Files, f)
		}
	}
	if len(b.Files) == 0 {
		return Built{}, nil
	}
	enc, err := zstd.NewWriter(out,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithWindowSize(zstdWindow))
	if err != nil {
		return Built{}, err
	}
	tw := tar.NewWriter(enc)
	for _, f := range b.Files {
		n, err := addMember(tw, base, f, opts)
		if err != nil {
			return Built{}, fmt.Errorf("%s: %w", f, err)
		}
		b.Redactions += n
	}
	if err := tw.Close(); err != nil {
		return Built{}, err
	}
	if err := enc.Close(); err != nil {
		return Built{}, err
	}
	return b, nil
}

// addMember writes one file into the tar and returns how many tokens were
// redacted from it. A file deleted after selection is skipped.
func addMember(tw *tar.Writer, base, f string, opts Options) (int, error) {
	path := filepath.Join(base, filepath.FromSlash(f))
	st, err := os.Stat(path)
	if err != nil {
		return 0, nil // deleted after selection
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	hdr := &tar.Header{
		Name:    f,
		Mode:    0o644,
		Size:    st.Size(),
		ModTime: st.ModTime(),
		Format:  tar.FormatPAX,
	}
	// A transcript can grow while it's read (an active session); take
	// exactly the size it had when selected so the tar stays well formed.
	body := io.LimitReader(file, st.Size())
	if !opts.Redact || !redactable[strings.ToLower(filepath.Ext(f))] {
		if err := tw.WriteHeader(hdr); err != nil {
			return 0, err
		}
		if n, err := io.Copy(tw, body); err != nil {
			return 0, err
		} else if n != st.Size() {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, nil
	}
	// Redacting changes the size the header carries, so the member is read
	// whole first.
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, err
	}
	if int64(len(data)) != st.Size() {
		return 0, io.ErrUnexpectedEOF
	}
	data, n := redact.Bytes(data)
	hdr.Size = int64(len(data))
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	_, err = tw.Write(data)
	return n, err
}

// BuildFile writes a week's tarball to a path.
func BuildFile(base string, files []string, out string, opts Options) (Built, int64, error) {
	f, err := os.Create(out)
	if err != nil {
		return Built{}, 0, err
	}
	b, err := Build(base, files, f, opts)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || len(b.Files) == 0 {
		os.Remove(out)
		return Built{}, 0, err
	}
	st, err := os.Stat(out)
	if err != nil {
		return Built{}, 0, err
	}
	return b, st.Size(), nil
}
