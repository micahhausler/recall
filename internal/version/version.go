// Package version holds the build version, set by the release ldflags.
package version

import "strings"

// Version is "dev" for local builds; releases set it to the tag, v included.
var Version = "dev"

// Number is Version without the tag's leading v, for User-Agent headers.
func Number() string { return strings.TrimPrefix(Version, "v") }

// UserAgentAppID identifies recall in AWS request User-Agent headers, as
// app/<id>, so bucket access logs and CloudTrail say which tool called.
func UserAgentAppID() string {
	return "recall-" + Number()
}
