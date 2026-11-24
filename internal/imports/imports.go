package imports

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
	importsv1 "github.com/micahhausler/recall/api/imports/v1"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/hook"
	"github.com/micahhausler/recall/internal/redact"
)

// sinceSlack is how far before the last complete run's start the next
// run asks from, for clock skew between this machine and the source.
const sinceSlack = time.Hour

// Options configure one import run.
type Options struct {
	// Name is the harness name, and Host this machine's.
	Name, Host string
	// Dir is the harness's import directory: state, lock, and run
	// directories. Sessions is where stored sessions go, the harness's
	// source.
	Dir, Sessions string
	// Full asks for everything, Replace accepts a shorter copy of a
	// stored session, and DryRun stores nothing and leaves since alone.
	Full, Replace, DryRun bool
	// Limits bound the output; zero is importsv1.DefaultLimits.
	Limits importsv1.Limits
	// Now is when the run starts.
	Now time.Time
	// LockStale is how old a lock must be to be taken over, longer than
	// any run.
	LockStale time.Duration
}

// Result is what one run did.
type Result struct {
	// Since is what the request asked from; empty for everything.
	Since string
	// Status is the plugin's answer, and Found the session files recall
	// found.
	Status importsv1.Status
	Found  int
	// Stored are the sessions written, Unchanged the ones identical to
	// what was stored, and Held the shorter copies kept back.
	Stored    []string
	Unchanged int
	Held      []Held
	// Rejected are the files refused, and Skipped the entries that
	// weren't regular *.jsonl files.
	Rejected []Rejected
	Skipped  []string
	// Dropped counts events of a kind this recall doesn't know.
	Dropped int
	// Redactions counts secret-shaped tokens replaced.
	Redactions int
	// SinceMoved reports whether the next run asks from this run's start.
	SinceMoved bool
}

// Held is a session whose new copy has fewer events than the stored one.
type Held struct {
	ID   string
	Drop int
}

// Rejected is one session file refused, and why.
type Rejected struct {
	File, Reason string
}

// State is what an import keeps between runs, in state.json.
type State struct {
	// Since is the start of the last run that finished complete.
	Since time.Time `json:"since,omitzero"`
	// Last is the last run's outcome.
	Last *Outcome `json:"last,omitempty"`
}

// Outcome is one run's result as state.json records it.
type Outcome struct {
	Start     time.Time `json:"start"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	Partial   bool      `json:"partial,omitempty"`
	Stored    int       `json:"stored"`
	Unchanged int       `json:"unchanged"`
	Held      int       `json:"held,omitempty"`
	Rejected  int       `json:"rejected,omitempty"`
}

// ErrLocked is returned when another import of the same harness holds the
// lock.
var ErrLocked = errors.New("another import of this harness is running")

// ReadState reads an import's state; a missing file is the zero state.
func ReadState(dir string) (State, error) {
	var s State
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", filepath.Join(dir, "state.json"), err)
	}
	return s, nil
}

func writeState(dir string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "state.json"), dir, append(data, '\n'))
}

// writeAtomic writes data to path through a temp file in tmpDir and one
// rename, mode 0600.
func writeAtomic(path, tmpDir string, data []byte) error {
	f, err := os.CreateTemp(tmpDir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Run runs one import: it takes the harness's lock, runs src into a fresh
// output directory, and stores every session that checks out. A failed
// plugin run stores nothing and leaves since where it was.
func Run(ctx context.Context, src Source, o Options) (*Result, error) {
	if o.Limits == (importsv1.Limits{}) {
		o.Limits = importsv1.DefaultLimits
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	release, ok := hook.Lock(filepath.Join(o.Dir, "lock"), o.LockStale, o.Now)
	if !ok {
		return nil, ErrLocked
	}
	defer release()
	// A run that crashed left its output directory behind.
	if old, err := filepath.Glob(filepath.Join(o.Dir, "run-*")); err == nil {
		for _, d := range old {
			os.RemoveAll(d)
		}
	}
	state, err := ReadState(o.Dir)
	if err != nil {
		return nil, err
	}
	out, err := os.MkdirTemp(o.Dir, "run-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(out)
	res := &Result{}
	if !o.Full && !state.Since.IsZero() {
		res.Since = state.Since.Add(-sinceSlack).UTC().Format(time.RFC3339)
	}
	record := func(err error) error {
		if o.DryRun {
			return err
		}
		last := &Outcome{Start: o.Now.UTC(), OK: err == nil, Partial: res.Status.Partial,
			Stored: len(res.Stored), Unchanged: res.Unchanged, Held: len(res.Held), Rejected: len(res.Rejected)}
		if err != nil {
			last.Error = err.Error()
		}
		state.Last = last
		if res.SinceMoved {
			state.Since = o.Now.UTC()
		}
		if werr := writeState(o.Dir, state); werr != nil && err == nil {
			err = werr
		}
		return err
	}
	status, err := src.Fetch(ctx, importsv1.Spec{Name: o.Name, OutputDir: out, Since: res.Since, Limits: o.Limits})
	if err != nil {
		return res, record(err)
	}
	res.Status = status
	if err := ingest(out, o, res); err != nil {
		return res, record(err)
	}
	res.SinceMoved = !status.Partial && !o.DryRun
	return res, record(nil)
}

// session is one validated session file, ready to store.
type session struct {
	file, id string
	events   int
	body     []byte
}

// ingest validates every file in out and stores the sessions that pass.
// Only an output too large or with too many files is an error, and then
// nothing is stored.
func ingest(out string, o Options, res *Result) error {
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	type candidate struct {
		name string
		info os.FileInfo
	}
	var files []candidate
	var total int64
	for _, d := range entries {
		info, err := os.Lstat(filepath.Join(out, d.Name()))
		if err != nil || !info.Mode().IsRegular() || !strings.HasSuffix(d.Name(), ".jsonl") {
			res.Skipped = append(res.Skipped, d.Name())
			continue
		}
		files = append(files, candidate{d.Name(), info})
		total += info.Size()
	}
	res.Found = len(files)
	if len(files) > o.Limits.Sessions {
		return fmt.Errorf("the plugin wrote %d session files, over the limit of %d; nothing was stored", len(files), o.Limits.Sessions)
	}
	if total > o.Limits.TotalBytes {
		return fmt.Errorf("the plugin wrote %d bytes, over the limit of %d; nothing was stored", total, o.Limits.TotalBytes)
	}
	byID := map[string][]*session{}
	var ids []string
	for _, c := range files {
		s, dropped, redactions, err := readSession(filepath.Join(out, c.name), c.info, o)
		if err != nil {
			res.Rejected = append(res.Rejected, Rejected{File: c.name, Reason: err.Error()})
			continue
		}
		res.Dropped += dropped
		res.Redactions += redactions
		if byID[s.id] == nil {
			ids = append(ids, s.id)
		}
		byID[s.id] = append(byID[s.id], s)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ss := byID[id]
		if len(ss) > 1 {
			for _, s := range ss {
				res.Rejected = append(res.Rejected, Rejected{File: s.file, Reason: fmt.Sprintf("session %q is in %d files", id, len(ss))})
			}
			continue
		}
		if err := store(ss[0], out, o, res); err != nil {
			return err
		}
	}
	sort.Slice(res.Rejected, func(i, j int) bool { return res.Rejected[i].File < res.Rejected[j].File })
	return nil
}

// readSession reads and checks one session file: regular and still the
// file inspected, every line within limits and a JSON event, and the
// events one well-formed session. recall writes harness and host, redacts
// text and input, and cuts input; events of an unknown kind are dropped.
func readSession(path string, inspected os.FileInfo, o Options) (*session, int, int, error) {
	// O_NONBLOCK: a FIFO swapped in after the Lstat can't block the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	if !st.Mode().IsRegular() || !os.SameFile(st, inspected) {
		return nil, 0, 0, errors.New("the file changed between being listed and opened")
	}
	if st.Size() > o.Limits.SessionBytes {
		return nil, 0, 0, fmt.Errorf("%d bytes, over the limit of %d", st.Size(), o.Limits.SessionBytes)
	}
	sc := bufio.NewScanner(io.LimitReader(f, o.Limits.SessionBytes+1))
	sc.Buffer(make([]byte, 0, min(64<<10, o.Limits.LineBytes)), int(o.Limits.LineBytes)+1)
	var evs []eventsv1.Event
	var read int64
	dropped, redactions, line := 0, 0, 0
	for sc.Scan() {
		line++
		read += int64(len(sc.Bytes())) + 1
		if read > o.Limits.SessionBytes+1 {
			return nil, 0, 0, fmt.Errorf("over the limit of %d bytes", o.Limits.SessionBytes)
		}
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var e eventsv1.Event
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, 0, 0, fmt.Errorf("line %d isn't a JSON event: %v", line, err)
		}
		if !e.Kind.Known() {
			dropped++
			continue
		}
		e.Harness, e.Host = o.Name, o.Host
		var n int
		e.Text, n = redact.String(e.Text)
		redactions += n
		e.Input, n = redact.String(e.Input)
		redactions += n
		e.Input = eventsv1.TrimInput(e.Input)
		evs = append(evs, e)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, 0, 0, fmt.Errorf("line %d is over the limit of %d bytes", line+1, o.Limits.LineBytes)
		}
		return nil, 0, 0, err
	}
	if len(evs) == 0 {
		return nil, 0, 0, errors.New("no events of a kind recall knows")
	}
	if err := eventsv1.CheckSession(evs); err != nil {
		return nil, 0, 0, err
	}
	id := evs[0].Session
	if !importsv1.ValidSessionID(id) {
		return nil, 0, 0, fmt.Errorf("session id %q isn't 1 to 128 letters, digits, dots, dashes, or underscores, starting with a letter or digit, with no \"..\"", id)
	}
	var buf bytes.Buffer
	if err := events.WriteJSONL(&buf, evs); err != nil {
		return nil, 0, 0, err
	}
	return &session{file: filepath.Base(path), id: id, events: len(evs), body: buf.Bytes()}, dropped, redactions, nil
}

// store writes one session to <Sessions>/<id>.jsonl unless the stored copy
// is identical, or longer and Replace is off.
func store(s *session, out string, o Options, res *Result) error {
	target := filepath.Join(o.Sessions, s.id+".jsonl")
	if rel, err := filepath.Rel(o.Sessions, target); err != nil || rel != s.id+".jsonl" {
		res.Rejected = append(res.Rejected, Rejected{File: s.file, Reason: fmt.Sprintf("session %q doesn't name a file in %s", s.id, o.Sessions)})
		return nil
	}
	old, err := os.ReadFile(target)
	switch {
	case err == nil && bytes.Equal(old, s.body):
		res.Unchanged++
		return nil
	case err == nil && !o.Replace:
		if had := bytes.Count(old, []byte("\n")); s.events < had {
			res.Held = append(res.Held, Held{ID: s.id, Drop: had - s.events})
			return nil
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	res.Stored = append(res.Stored, s.id)
	if o.DryRun {
		return nil
	}
	if err := os.MkdirAll(o.Sessions, 0o700); err != nil {
		return err
	}
	// The run directory is beside sessions/ by default, so this is one
	// rename; a source elsewhere gets its temp file beside the target.
	if err := writeAtomic(target, out, s.body); err != nil {
		var linkErr *os.LinkError
		if !errors.As(err, &linkErr) {
			return err
		}
		return writeAtomic(target, o.Sessions, s.body)
	}
	return nil
}
