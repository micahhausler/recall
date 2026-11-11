package harness

import (
	"strings"
	"testing"
)

func TestParseInit(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"hook_started"}`,
		`not json`,
		`{"type":"system","subtype":"init","skills":["keep","tools:envs"],"slash_commands":["keep","tools:envs","clear","compact"],"terminal_slash_commands":["doctor"]}`,
		`{"type":"assistant"}`,
	}, "\n")
	inv, err := parseInit(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(inv.Skills, ","); got != "keep,tools:envs" {
		t.Errorf("skills = %s", got)
	}
	if got := strings.Join(inv.Commands, ","); got != "keep,tools:envs,clear,compact,doctor" {
		t.Errorf("commands = %s", got)
	}
}

func TestParseInitMissing(t *testing.T) {
	if _, err := parseInit(strings.NewReader(`{"type":"assistant"}`)); err == nil {
		t.Error("want an error without an init event")
	}
}
