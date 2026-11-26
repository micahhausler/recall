package version

import "testing"

func TestNumberDropsTheTagsV(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	for v, want := range map[string]string{"v0.4.5": "0.4.5", "dev": "dev"} {
		Version = v
		if got := Number(); got != want {
			t.Errorf("Number() with %q = %q, want %q", v, got, want)
		}
		if got := UserAgentAppID(); got != "recall-"+want {
			t.Errorf("UserAgentAppID() with %q = %q", v, got)
		}
	}
}
