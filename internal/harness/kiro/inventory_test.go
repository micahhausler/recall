package kiro

import (
	"strings"
	"testing"
)

func TestParseCommands(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"runStarted","data":{"payloadSchema":"acp"}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"keep","_meta":{"kiro":{"type":"skill"}}}]}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"builder","_meta":{"kiro":{"type":"custom-agent"}}},{"name":"keep","_meta":{"kiro":{"type":"skill"}}},{"name":"explain","_meta":{"kiro":{"type":"prompt"}}}]}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"session_info_update","_meta":{"kiro":{"kind":"turn_start"}}}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"available_commands_update","availableCommands":[]}}}`,
	}, "\n")
	inv, session, err := parseCommands(strings.NewReader(stream))
	if err != nil || session != "sess_a" {
		t.Fatalf("session %q, err %v", session, err)
	}
	if got := strings.Join(inv.Skills, ","); got != "keep" {
		t.Errorf("skills = %s", got)
	}
	if got := strings.Join(inv.Commands, ","); got != "builder,keep,explain" {
		t.Errorf("commands = %s", got)
	}
}

func TestParseCommandsError(t *testing.T) {
	_, _, err := parseCommands(strings.NewReader(`{"type":"runError","data":{"sessionId":null,"stage":"auth","message":"Not logged in."}}`))
	if err == nil || err.Error() != "Not logged in." {
		t.Errorf("err = %v", err)
	}
}
