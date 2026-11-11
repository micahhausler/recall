package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// zstdWindow is 2^27: the long-distance window finds repeats across the many
// similar JSONL lines, and 2^27 is within zstd's default decompression limit,
// so plain `zstd -d` reads the output without flags.
const zstdWindow = 1 << 27

// Build tars the files (relative to base) and zstd-compresses them to out.
// Compression matches `zstd -3 --long=27` closely enough to keep the archive
// layout readable by plain zstd. Files deleted since selection (a harness's
// own cleanup can race a run) are dropped rather than failing; the files
// actually archived are returned, and no output file is written when every
// file vanished.
func Build(base string, files []string, out io.Writer) ([]string, error) {
	kept := make([]string, 0, len(files))
	for _, f := range files {
		if st, err := os.Lstat(filepath.Join(base, filepath.FromSlash(f))); err == nil && st.Mode().IsRegular() {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	enc, err := zstd.NewWriter(out,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithWindowSize(zstdWindow))
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(enc)
	for _, f := range kept {
		path := filepath.Join(base, filepath.FromSlash(f))
		st, err := os.Stat(path)
		if err != nil {
			continue // deleted after selection
		}
		hdr := &tar.Header{
			Name:    f,
			Mode:    0o644,
			Size:    st.Size(),
			ModTime: st.ModTime(),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		// A transcript can grow while it's read (an active session); copy
		// exactly the size in the header so the tar stays well formed.
		if _, err := io.CopyN(tw, file, st.Size()); err != nil {
			file.Close()
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		file.Close()
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return kept, nil
}

// BuildFile writes a week's tarball to a path.
func BuildFile(base string, files []string, out string) ([]string, int64, error) {
	f, err := os.Create(out)
	if err != nil {
		return nil, 0, err
	}
	kept, err := Build(base, files, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || len(kept) == 0 {
		os.Remove(out)
		return nil, 0, err
	}
	st, err := os.Stat(out)
	if err != nil {
		return nil, 0, err
	}
	return kept, st.Size(), nil
}
