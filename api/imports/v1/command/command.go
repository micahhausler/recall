// Package command is an import plugin's side of the recall.import/v1
// contract (docs/import.md): read the request recall set in the
// environment, write one session file per session into the output
// directory, and print the answer. A plugin written in Go calls Run with
// a function that fetches its sessions, and that function is the only
// part the plugin writes itself.
//
// Nothing here knows where sessions come from. The package reads no
// environment: the request's text and the writer are arguments, so a
// plugin's tests pass a string, a temp directory, and a buffer.
package command

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
	importsv1 "github.com/micahhausler/recall/api/imports/v1"
)

// Request is one import request, read and checked.
type Request struct {
	// Spec is the request's spec as this package read it.
	Spec importsv1.Spec
	// Name is the harness name.
	Name string
	// OutputDir is where session files go.
	OutputDir string
	// Since is the time to return changes from; zero asks for everything.
	Since time.Time
	// Limits are the request's limits, with recall's defaults for any it
	// left out.
	Limits importsv1.Limits
}

// ParseRequest reads and checks a request. info is the value of the
// environment variable importsv1.InfoEnv. An empty value, text that isn't
// a JSON object, another apiVersion or kind, a missing spec or output
// directory, and a since that isn't RFC 3339 are each an error that says
// which. Fields this version doesn't know are ignored.
func ParseRequest(info string) (Request, error) {
	if info == "" {
		return Request{}, fmt.Errorf("%s isn't set; recall sets it to the import request when it runs this plugin", importsv1.InfoEnv)
	}
	var in importsv1.Import
	if err := json.Unmarshal([]byte(info), &in); err != nil {
		return Request{}, fmt.Errorf("reading the import request from %s: %w", importsv1.InfoEnv, err)
	}
	if in.APIVersion != importsv1.APIVersion || in.Kind != importsv1.Kind {
		return Request{}, fmt.Errorf("the request is %q %q; this plugin answers %q %q", in.APIVersion, in.Kind, importsv1.APIVersion, importsv1.Kind)
	}
	if in.Spec == nil {
		return Request{}, errors.New("the request has no spec")
	}
	if !filepath.IsAbs(in.Spec.OutputDir) {
		return Request{}, fmt.Errorf("spec.outputDir %q isn't an absolute path", in.Spec.OutputDir)
	}
	req := Request{Spec: *in.Spec, Name: in.Spec.Name, OutputDir: in.Spec.OutputDir, Limits: in.Spec.Limits}
	if in.Spec.Since != "" {
		t, err := time.Parse(time.RFC3339, in.Spec.Since)
		if err != nil {
			return Request{}, fmt.Errorf("spec.since %q isn't RFC 3339", in.Spec.Since)
		}
		req.Since = t
	}
	def := importsv1.DefaultLimits
	if req.Limits.Sessions <= 0 {
		req.Limits.Sessions = def.Sessions
	}
	if req.Limits.SessionBytes <= 0 {
		req.Limits.SessionBytes = def.SessionBytes
	}
	if req.Limits.LineBytes <= 0 {
		req.Limits.LineBytes = def.LineBytes
	}
	if req.Limits.TotalBytes <= 0 {
		req.Limits.TotalBytes = def.TotalBytes
	}
	return req, nil
}

// Output is where a plugin writes its sessions, one at a time.
type Output struct {
	req      Request
	sessions int
	filtered int
	partial  bool
	total    int64
	ids      map[string]bool
	open     *Session
}

// Session starts the file for one session. It refuses an id recall would
// refuse, an id already written this run, a session past the request's
// limit, and a second session while one is open.
func (o *Output) Session(id string) (*Session, error) {
	if o.open != nil {
		return nil, fmt.Errorf("session %q is still open; close it first", o.open.id)
	}
	if !importsv1.ValidSessionID(id) {
		return nil, fmt.Errorf("session id %q isn't 1 to 128 letters, digits, dots, dashes, or underscores, starting with a letter or digit, with no \"..\"", id)
	}
	if o.ids[id] {
		return nil, fmt.Errorf("session %q was already written this run", id)
	}
	if o.sessions >= o.req.Limits.Sessions {
		return nil, fmt.Errorf("the request allows at most %d sessions", o.req.Limits.Sessions)
	}
	path := filepath.Join(o.req.OutputDir, id+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if o.ids == nil {
		o.ids = map[string]bool{}
	}
	o.ids[id] = true
	s := &Session{o: o, id: id, path: path, f: f, w: bufio.NewWriter(f), seen: map[string]bool{}}
	o.open = s
	return s, nil
}

// Filtered counts n sessions the plugin left out on purpose.
func (o *Output) Filtered(n int) { o.filtered += n }

// Partial marks the run as not having fetched everything it was asked
// for, so recall asks again from the same point next run.
func (o *Output) Partial() { o.partial = true }

// Session is one session file being written.
type Session struct {
	o      *Output
	id     string
	path   string
	f      *os.File
	w      *bufio.Writer
	size   int64
	n      int
	dated  bool
	seen   map[string]bool
	failed error
}

// Add writes one event. An empty Session is the session's id and an empty
// Harness the request's name. It refuses an event that fails
// eventsv1.Check, a parent_id that names no earlier message, another
// session's id, and an event past the request's size limits; after a
// refusal the session can only be closed, which discards it.
func (s *Session) Add(e eventsv1.Event) error {
	if s.failed != nil {
		return fmt.Errorf("session %q: an earlier event was refused: %w", s.id, s.failed)
	}
	if s.f == nil {
		return fmt.Errorf("session %q is closed", s.id)
	}
	err := s.add(e)
	if err != nil {
		s.failed = err
		err = fmt.Errorf("session %q event %d: %w", s.id, s.n+1, err)
	}
	return err
}

func (s *Session) add(e eventsv1.Event) error {
	if e.Session == "" {
		e.Session = s.id
	}
	if e.Session != s.id {
		return fmt.Errorf("session is %q", e.Session)
	}
	if e.Harness == "" {
		e.Harness = s.o.req.Name
	}
	if err := eventsv1.Check(e); err != nil {
		return err
	}
	if e.ParentID != "" && !s.seen[e.ParentID] {
		return fmt.Errorf("parent_id %q names no earlier message", e.ParentID)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	n := int64(len(line))
	limits := s.o.req.Limits
	switch {
	case n > limits.LineBytes:
		return fmt.Errorf("the line is %d bytes, over the request's %d", n, limits.LineBytes)
	case s.size+n > limits.SessionBytes:
		return fmt.Errorf("the session would pass the request's %d bytes", limits.SessionBytes)
	case s.o.total+n > limits.TotalBytes:
		return fmt.Errorf("the run would pass the request's %d bytes", limits.TotalBytes)
	}
	if _, err := s.w.Write(line); err != nil {
		return err
	}
	s.size += n
	s.o.total += n
	s.n++
	s.dated = s.dated || e.Timestamp != ""
	if e.MessageID != "" {
		s.seen[e.MessageID] = true
	}
	return nil
}

// Close finishes the session file. A session with no events, with no
// dated event, or with a refused event is removed instead, and Close says
// why; the plugin can go on to the next session.
func (s *Session) Close() error {
	if s.f == nil {
		return nil
	}
	s.o.open = nil
	flushErr := s.w.Flush()
	closeErr := s.f.Close()
	s.f = nil
	var err error
	switch {
	case s.failed != nil:
		err = fmt.Errorf("session %q was discarded: %w", s.id, s.failed)
	case s.n == 0:
		err = fmt.Errorf("session %q has no events, so it was discarded", s.id)
	case !s.dated:
		err = fmt.Errorf("session %q has no event with a timestamp, so it was discarded", s.id)
	case flushErr != nil:
		err = flushErr
	case closeErr != nil:
		err = closeErr
	}
	if err != nil {
		s.o.total -= s.size
		os.Remove(s.path)
		return err
	}
	s.o.sessions++
	return nil
}

// Run answers one request: it parses info, calls fetch with ctx, the
// request, and an Output in the request's directory, and writes the
// answer to stdout. It is a whole import plugin but for where sessions
// come from, which is fetch.
//
// info is the value of the environment variable importsv1.InfoEnv, and
// stdout is where recall reads the answer. Run returns fetch's error as it
// is, and a session fetch left open is an error too. Nothing is written
// to stdout unless every step succeeded, so on an error the plugin only
// has to print it to stderr and exit non-zero.
func Run(ctx context.Context, info string, stdout io.Writer, fetch func(context.Context, Request, *Output) error) error {
	req, err := ParseRequest(info)
	if err != nil {
		return err
	}
	o := &Output{req: req}
	err = fetch(ctx, req, o)
	if open := o.open; open != nil {
		open.Close()
		if err == nil {
			err = fmt.Errorf("session %q was never closed", open.id)
		}
	}
	if err != nil {
		return err
	}
	out, err := json.Marshal(importsv1.Import{
		APIVersion: importsv1.APIVersion, Kind: importsv1.Kind,
		Status: &importsv1.Status{Sessions: o.sessions, Filtered: o.filtered, Partial: o.partial},
	})
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(out, '\n'))
	return err
}
