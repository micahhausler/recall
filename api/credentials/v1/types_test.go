package v1

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The JSON here is the contract. A change to this test is a change to
// every credential command already written.
func TestWireFormat(t *testing.T) {
	const request = `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/example"}}`
	got, err := json.Marshal(Credential{APIVersion: APIVersion, Kind: Kind, Spec: Spec{Repo: "github.com/you/example"}})
	if err != nil || string(got) != request {
		t.Errorf("request = %s, %v\nwant      %s", got, err, request)
	}

	const answer = `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/example"},` +
		`"status":{"token":"ghs_example","expirationTimestamp":"2026-03-02T13:00:00Z",` +
		`"commitIdentity":{"name":"recall-you[bot]","email":"4242+recall-you[bot]@users.noreply.github.com"},"owner":"you"}}`
	expires := time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC)
	full := Credential{
		APIVersion: APIVersion, Kind: Kind, Spec: Spec{Repo: "github.com/you/example"},
		Status: &Status{
			Token: "ghs_example", ExpirationTimestamp: &expires,
			CommitIdentity: &CommitIdentity{Name: "recall-you[bot]", Email: "4242+recall-you[bot]@users.noreply.github.com"},
			Owner:          "you",
		},
	}
	got, err = json.Marshal(full)
	if err != nil || string(got) != answer {
		t.Errorf("answer = %s, %v\nwant     %s", got, err, answer)
	}
	var back Credential
	if err := json.Unmarshal([]byte(answer), &back); err != nil {
		t.Fatal(err)
	}
	if back.Status == nil || back.Status.Token != "ghs_example" || !back.Status.ExpirationTimestamp.Equal(expires) ||
		*back.Status.CommitIdentity != *full.Status.CommitIdentity || back.Status.Owner != "you" || back.Spec != full.Spec {
		t.Errorf("read back %+v", back)
	}
}

// Only the token is required: an answer with nothing else says nothing
// else, and reads back with the rest unset.
func TestOptionalFieldsAreLeftOut(t *testing.T) {
	got, err := json.Marshal(Status{Token: "ghs_example"})
	if err != nil || string(got) != `{"token":"ghs_example"}` {
		t.Errorf("status = %s, %v", got, err)
	}
	var back Credential
	if err := json.Unmarshal([]byte(`{"status":{"token":"ghs_example","later":"field"},"more":1}`), &back); err != nil {
		t.Fatalf("an object with fields this version doesn't know: %v", err)
	}
	if s := back.Status; s == nil || s.ExpirationTimestamp != nil || s.CommitIdentity != nil || s.Owner != "" {
		t.Errorf("read back %+v", back.Status)
	}
}

func TestFormattingLeavesTheTokenOut(t *testing.T) {
	expires := time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC)
	s := Status{Token: "ghs_example", ExpirationTimestamp: &expires, Owner: "you"}
	c := Credential{APIVersion: APIVersion, Kind: Kind, Status: &s}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{s, &s, c, &c, []Status{s}} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, "ghs_example") {
				t.Errorf("%s of %T printed the token: %s", verb, v, out)
			}
		}
	}
	if out := fmt.Sprint(s); !strings.Contains(out, "Token: redacted") || !strings.Contains(out, "2026-03-02T13:00:00Z") {
		t.Errorf("status prints as %s", out)
	}
	if out := fmt.Sprint(Status{}); !strings.Contains(out, "Token: none") {
		t.Errorf("empty status prints as %s", out)
	}
}
