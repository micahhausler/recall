// Package version holds the build version, set by the release ldflags.
package version

// Version is "dev" for local builds; releases set it to the tag.
var Version = "dev"

// UserAgentAppID identifies recall in AWS request User-Agent headers, as
// app/<id>, so bucket access logs and CloudTrail say which tool called.
func UserAgentAppID() string {
	return "recall-" + Version
}
