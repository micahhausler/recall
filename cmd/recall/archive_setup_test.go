package main

import (
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/config"
)

// Setup creates in the account regional namespace only a name in that form for the configured region.
func TestArchiveSetupNamespaced(t *testing.T) {
	for bucket, want := range map[string]bool{
		"chat-archive-123456789012-us-west-2-an": true,
		"mine-123456789012-us-west-2-an":         true,
		"team-backup-123456789012-archive-an":    false,
		"chat-archive-123456789012-us-east-1-an": false,
		"mine":                                   false,
	} {
		s := newArchiveSetup(config.S3Settings{Region: "us-west-2", Bucket: bucket, RetentionDays: 3650}, nil, bucket)
		if s.Namespaced != want || s.Bucket != bucket || s.Region != "us-west-2" || s.Retention != 3650 {
			t.Errorf("%s: setup = %+v, want namespaced %v", bucket, s, want)
		}
	}
}

// --lookback takes 1 or more; 0 and below are usage errors before anything runs.
func TestLookbackZeroIsUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"archive", "--lookback", "0"},
		{"archive", "--lookback", "-1"},
		{"archive-chats", "run", "--lookback", "0"},
		{"archive-chats", "--lookback", "0"},
	} {
		_, ac := recordExec(t)
		code, _, errText := capture(t, args...)
		if code != 2 || len(*ac) != 0 || !strings.Contains(errText, "--lookback must be at least 1") {
			t.Errorf("recall %v = %d, %q, ran %v; want 2 and nothing run", args, code, errText, *ac)
		}
	}
	_, ac := recordExec(t)
	if code, _, errText := capture(t, "archive", "--lookback", "1"); code != 0 || len(*ac) != 1 || (*ac)[0].o.lookback != 1 {
		t.Errorf("--lookback 1 = %d, %q, %v", code, errText, *ac)
	}
}
