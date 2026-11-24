package harness

import (
	"io"

	"github.com/micahhausler/recall/internal/events"
)

// formatted is a harness recall doesn't know by name whose transcripts are
// in a known harness's format, such as Claude Code run with another config
// directory. It reads through that adapter and names itself, and its
// events, by its own name, so its archives and observations stay apart.
type formatted struct {
	Harness
	name string
}

func (f *formatted) Name() string { return f.name }

func (f *formatted) ReadSessionFile(path, host string) ([]events.Event, error) {
	evs, err := f.Harness.ReadSessionFile(path, host)
	return f.own(evs), err
}

func (f *formatted) ReadSession(r io.Reader, session, host string) ([]events.Event, error) {
	evs, err := f.Harness.ReadSession(r, session, host)
	return f.own(evs), err
}

func (f *formatted) own(evs []events.Event) []events.Event {
	for i := range evs {
		evs[i].Harness = f.name
	}
	return evs
}

// MetadataSession and ReadSessionMetadata pass through to an adapter that
// reads session metadata, so an archive read still recovers the cwd.
func (f *formatted) MetadataSession(path string) (string, bool) {
	if md, ok := f.Harness.(SessionMetadata); ok {
		return md.MetadataSession(path)
	}
	return "", false
}

func (f *formatted) ReadSessionMetadata(r io.Reader) string {
	if md, ok := f.Harness.(SessionMetadata); ok {
		return md.ReadSessionMetadata(r)
	}
	return ""
}
