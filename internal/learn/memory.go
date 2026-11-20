package learn

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/redact"
	"github.com/micahhausler/recall/internal/store"
)

// CategoryMemory marks an observation read from a harness's own memory: a
// note the agent wrote for itself on one machine. It needs no model call to
// read, and it is never owner-typed. A lesson that shows up there and in
// sessions is one worth promoting into a reviewed, portable context file.
const CategoryMemory = "memory"

// memoryBudget is how much of a note an observation keeps.
const memoryBudget = 300

// memoryText is one line for a note: its description, or the start of its
// body when it has none, with the harness's own kind in front.
func memoryText(m harness.Memory) string {
	text := m.Description
	if text == "" {
		text, _, _ = strings.Cut(m.Body, "\n")
	}
	text = strings.Join(strings.Fields(text), " ")
	text, _ = redact.String(text)
	if r := []rune(text); len(r) > memoryBudget {
		text = string(r[:memoryBudget]) + "…"
	}
	if text == "" {
		return ""
	}
	if m.Type != "" {
		return "(" + m.Type + ") " + text
	}
	return text
}

// MemoryRecord turns one native memory note into an observation record,
// dated by when the note last changed and placed where its project ran.
// ok is false for a note with nothing to say.
func MemoryRecord(m harness.Memory, harnessName, host, home string, locate Locator, now time.Time) (so SessionObservations, ok bool) {
	text := memoryText(m)
	if text == "" {
		return so, false
	}
	modified := m.Modified.UTC().Format(time.RFC3339)
	so = SessionObservations{
		Harness: harnessName, Host: host,
		// Its own key, never the origin session's: a note must not replace
		// that session's observations as the newest record. Every machine
		// reuses this key for its own copy of the note; the record's
		// identity is the host and this key together, and ListObservations
		// dedupes on both, so one machine's note never shadows another's.
		Session:    "memory:" + m.Project + "/" + m.Name,
		ObservedAt: now.UTC().Format(time.RFC3339),
		FirstEvent: modified, LastEvent: modified,
		Observations: []Observation{{Category: CategoryMemory, Text: text, Origin: m.Session}},
	}
	if m.CWD != "" {
		so.Scope = Scope{CWD: homeRelative(m.CWD, home)}
		if locate != nil {
			if root, remote := locate(m.CWD); root != "" {
				so.Scope.Repo, so.Scope.Remote = homeRelative(root, home), remote
			}
		}
	}
	so.Fingerprint = shortHash(host, text, modified, m.Session, so.Scope.Key())
	return so, true
}

// PutMemories stores a record for each native memory note that isn't
// stored at its current content, and returns how many were new.
func PutMemories(ctx context.Context, st store.Store, mems []harness.Memory, harnessName, host, home string,
	locate Locator, now time.Time) (added int, err error) {
	for _, m := range mems {
		so, ok := MemoryRecord(m, harnessName, host, home, locate, now)
		if !ok {
			continue
		}
		key := fmt.Sprintf("analysis/%s/memory-%s-%s.json", harnessName, shortHash(host, m.Project, m.Name), so.Fingerprint)
		if _, err := st.Head(ctx, key); err == nil {
			continue
		}
		if err := putJSON(ctx, st, key, so); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}
