package review

import "testing"

func TestRemoteSlug(t *testing.T) {
	for in, want := range map[string]string{
		"git@example.com:you/project.git":             "example.com/you/project",
		"https://example.com/you/project.git":         "example.com/you/project",
		"https://you:secret@Example.com/you/project/": "example.com/you/project",
		"ssh://git@example.com:2222/you/project":      "example.com/you/project",
		"example.com:group/sub/project.git":           "example.com/group/sub/project",
		"/srv/git/project.git":                        "",
		"":                                            "",
	} {
		if got := RemoteSlug(in); got != want {
			t.Errorf("RemoteSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
