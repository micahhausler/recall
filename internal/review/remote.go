package review

import (
	"net/url"
	"regexp"
	"strings"
)

var scpRemoteRE = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// RemoteSlug normalizes a git remote URL to host/path: no scheme, no user or
// credentials, no port, no .git suffix. The same repo cloned over SSH on one
// machine and HTTPS on another gets one slug.
func RemoteSlug(remote string) string {
	remote = strings.TrimSpace(remote)
	var host, path string
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" {
		host, path = u.Hostname(), u.Path
	} else if m := scpRemoteRE.FindStringSubmatch(remote); m != nil {
		host, path = m[1], m[2]
	} else {
		return ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + path
}
