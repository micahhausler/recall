package learn

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/store"
)

// With no effort the cache keys are the ones a store mapped without effort
// already holds; any effort changes them, and each level differently.
func TestFingerprintEffort(t *testing.T) {
	const rendered = "[owner] clone it under the import path"
	if got, want := Fingerprint(rendered, model.Call{Model: "haiku"}), shortHash(rendered, mapPrompt, "haiku"); got != want {
		t.Errorf("Fingerprint without effort = %s, want %s", got, want)
	}
	shown := []RuleRef{{ID: "r-0001", TextHash: "abc"}}
	if got, want := HitsKey("claude", "s1", "c", model.Call{Model: "haiku"}, shown),
		hitsPrefix("claude", "s1")+shortHash("c", hitsPromptHash(), "haiku", "r-0001:abc")+".json"; got != want {
		t.Errorf("HitsKey without effort = %s, want %s", got, want)
	}
	seen := map[string]string{Fingerprint(rendered, model.Call{Model: "haiku"}): ""}
	for _, e := range config.Efforts(config.HarnessKiro) {
		fp := Fingerprint(rendered, model.Call{Model: "haiku", Effort: e})
		if prev, dup := seen[fp]; dup {
			t.Errorf("effort %q fingerprint %s matches effort %q", e, fp, prev)
		}
		seen[fp] = e
	}
}

// The map runs at its effort, records it and the resolved model, and a new
// effort re-observes and re-judges a session the way a new model does.
func TestMapEffortRecordedAndRemaps(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "Observation[context]: Repos live under a GOPATH-style tree.\nRule[r-0001]: followed\n",
		Usage: model.Usage{ModelID: "claude-haiku-full-id"}}
	rule := Rule{ID: "r-0001", Text: "Never push without asking.", TextHash: textHash("Never push without asking."), Created: "2026-01-01"}
	ctx := context.Background()
	evs := []events.Event{ev(events.UserPrompt, "ship it")}

	m := mapper(st, fake, []Rule{rule})
	res, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !res.Observed || !res.Judged {
		t.Fatalf("map without effort: err=%v res=%+v", err, res)
	}
	if res.Observations.Effort != "" || res.Observations.ModelID != "claude-haiku-full-id" {
		t.Errorf("observations effort %q, model_id %q", res.Observations.Effort, res.Observations.ModelID)
	}

	m = mapper(st, fake, []Rule{rule})
	m.Effort = "high"
	res, err = m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !res.Observed || !res.Judged {
		t.Fatalf("map with a new effort didn't re-map: err=%v res=%+v", err, res)
	}
	if call := fake.Calls[len(fake.Calls)-1]; call.Model != "haiku" || call.Effort != "high" {
		t.Errorf("map call = %+v, want haiku at high", call)
	}
	if o := res.Observations; o.Effort != "high" || o.ModelID != "claude-haiku-full-id" {
		t.Errorf("observations effort %q, model_id %q", o.Effort, o.ModelID)
	}
	if h := res.Hits; h.Effort != "high" || h.ModelID != "claude-haiku-full-id" {
		t.Errorf("hits effort %q, model_id %q", h.Effort, h.ModelID)
	}

	calls := len(fake.Calls)
	if res, err := m.MapSession(ctx, "claude", "s1", evs); err != nil || res.Ran() || len(fake.Calls) != calls {
		t.Errorf("same effort mapped again: err=%v res=%+v", err, res)
	}
}

func TestEffortJSONRoundTrip(t *testing.T) {
	so := SessionObservations{Fingerprint: "f", Harness: "claude", Session: "s1", Model: "haiku",
		Effort: "high", ModelID: "claude-haiku-full-id", Observations: []Observation{}}
	sh := SessionHits{Fingerprint: "f", Harness: "claude", Session: "s1", Model: "haiku",
		Effort: "high", ModelID: "claude-haiku-full-id", Rules: []RuleRef{}}
	for name, v := range map[string]any{"observations": so, "hits": sh} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["effort"] != "high" || fields["model_id"] != "claude-haiku-full-id" {
			t.Errorf("%s JSON = %s", name, data)
		}
	}
	var gotSO SessionObservations
	data, _ := json.Marshal(so)
	if err := json.Unmarshal(data, &gotSO); err != nil || gotSO.Effort != "high" || gotSO.ModelID != "claude-haiku-full-id" {
		t.Errorf("observations round trip = %+v, %v", gotSO, err)
	}
	var gotSH SessionHits
	data, _ = json.Marshal(sh)
	if err := json.Unmarshal(data, &gotSH); err != nil || gotSH.Effort != "high" || gotSH.ModelID != "claude-haiku-full-id" {
		t.Errorf("hits round trip = %+v, %v", gotSH, err)
	}

	// Files written without the fields still read.
	var oldSO SessionObservations
	if err := json.Unmarshal([]byte(`{"fingerprint":"f","harness":"claude","session":"s1","model":"haiku","observed_at":"2026-01-01T00:00:00Z","observations":[]}`), &oldSO); err != nil ||
		oldSO.Model != "haiku" || oldSO.Effort != "" || oldSO.ModelID != "" {
		t.Errorf("old observations = %+v, %v", oldSO, err)
	}
	var oldSH SessionHits
	if err := json.Unmarshal([]byte(`{"fingerprint":"f","harness":"claude","session":"s1","content":"c","prompt":"p","model":"haiku","judged_at":"2026-01-01T00:00:00Z","rules":[]}`), &oldSH); err != nil ||
		oldSH.Model != "haiku" || oldSH.Effort != "" || oldSH.ModelID != "" {
		t.Errorf("old hits = %+v, %v", oldSH, err)
	}
	// Without effort, neither field is written.
	data, _ = json.Marshal(SessionObservations{Model: "haiku"})
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	if _, ok := fields["effort"]; ok {
		t.Errorf("empty effort written: %s", data)
	}
}
