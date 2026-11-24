package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

// The JSON here is the contract. A change to this test is a change to
// every import plugin already written.
func TestWireFormat(t *testing.T) {
	const request = `{"apiVersion":"recall.import/v1","kind":"Import","spec":{"name":"chat",` +
		`"outputDir":"/home/you/.local/state/recall/import/chat/run-7f3c1a","since":"2026-03-01T05:00:00Z",` +
		`"limits":{"sessions":10000,"sessionBytes":67108864,"lineBytes":4194304,"totalBytes":1073741824}}}`
	got, err := json.Marshal(Import{APIVersion: APIVersion, Kind: Kind, Spec: &Spec{
		Name: "chat", OutputDir: "/home/you/.local/state/recall/import/chat/run-7f3c1a",
		Since: "2026-03-01T05:00:00Z", Limits: DefaultLimits,
	}})
	if err != nil || string(got) != request {
		t.Errorf("request = %s, %v\nwant      %s", got, err, request)
	}

	const answer = `{"apiVersion":"recall.import/v1","kind":"Import","status":{"sessions":12,"filtered":3,"partial":true}}`
	got, err = json.Marshal(Import{APIVersion: APIVersion, Kind: Kind, Status: &Status{Sessions: 12, Filtered: 3, Partial: true}})
	if err != nil || string(got) != answer {
		t.Errorf("answer = %s, %v\nwant     %s", got, err, answer)
	}
	var back Import
	if err := json.Unmarshal([]byte(`{"apiVersion":"recall.import/v1","kind":"Import","status":{"sessions":0,"newer":1},"newer":{}}`), &back); err != nil ||
		back.Status == nil || back.Status.Sessions != 0 {
		t.Errorf("an answer with unknown fields = %+v, %v", back, err)
	}
	if InfoEnv != "RECALL_IMPORT_INFO" {
		t.Errorf("InfoEnv = %q", InfoEnv)
	}
}

func TestValidSessionID(t *testing.T) {
	for id, want := range map[string]bool{
		"s-0001":                 true,
		"chat-c-0001":            true,
		"5f0c2a9e.1b7d_4c3a":     true,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
		"":                       false,
		"../etc":                 false,
		"a..b":                   false,
		"a/b":                    false,
		"a:b":                    false,
		".hidden":                false,
		"-flag":                  false,
		"space id":               false,
		"tab\tid":                false,
	} {
		if got := ValidSessionID(id); got != want {
			t.Errorf("ValidSessionID(%q) = %v, want %v", id, got, want)
		}
	}
}
