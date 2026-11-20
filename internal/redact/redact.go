// Package redact replaces secret-shaped tokens in text with a marker naming
// the kind, like [REDACTED:github-token].
//
// It matches only shapes that are almost never anything else: a vendor's
// token prefix, a PEM private key block, a credential header, a JWT, and a
// password in a position that names it, like user:pass@ in a URL, mysql's
// attached -p, curl -u, or a PASSWORD= assignment. It finds no password in
// plain prose and no key in a format it doesn't list, so it lowers what an
// archive holds and proves nothing about what's left.
//
// A replacement never adds a quote, a backslash, or a newline, and never
// runs from one JSON string into the next, so a JSONL line stays a JSONL
// line.
package redact

import (
	"bytes"
	"regexp"
)

// rule is one secret shape.
type rule struct {
	kind string
	re   *regexp.Regexp
	// needles, when set, are literals one of which every match contains; a
	// text holding none of them skips the regexp.
	needles [][]byte
	// keep, when set, is the capture group written back ahead of the marker:
	// what came before the token, or a header's name.
	keep int
	// secret, when set, is the capture group that must look generated (see
	// generated) for the match to count; it lets through words that only
	// share a token's prefix.
	secret int
	// tail, when set, is the capture group written back after the marker:
	// what closed the token, like the @ before a URL's host.
	tail int
}

// before is what a token starts after: the start of the text, a character
// no token holds, or a control escape inside a JSON string. A plain word
// boundary would miss the last: in "\nghp_..." the n touches the token.
const before = `(^|[^A-Za-z0-9_]|\\[nrtbf])`

// pemBody is what sits between a PEM block's BEGIN and END lines: base64,
// whitespace, and newlines as a JSON string escapes them, however many times
// over. It holds no quote and no dash, so a match ends at the block's own
// END line and can't run from one JSON string into another.
const pemBody = `(?:[A-Za-z0-9+/=]|\s|\\+[nrt])*`

var rules = []rule{
	{
		kind:    "private-key",
		re:      regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----` + pemBody + `-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
		needles: lits("PRIVATE KEY"),
	},
	{
		kind:    "aws-access-key",
		re:      regexp.MustCompile(before + `(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`),
		needles: lits("AKIA", "ASIA", "ABIA", "ACCA"),
		keep:    1,
	},
	{
		kind:    "github-token",
		re:      regexp.MustCompile(before + `(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})`),
		needles: lits("ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"),
		keep:    1,
	},
	{
		kind:    "slack-token",
		re:      regexp.MustCompile(before + `xox[abeoprs]-([A-Za-z0-9-]{10,})`),
		needles: lits("xox"),
		keep:    1,
		secret:  2,
	},
	{
		kind:    "api-key",
		re:      regexp.MustCompile(before + `sk-((?:ant-|proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,})`),
		needles: lits("sk-"),
		keep:    1,
		secret:  2,
	},
	{
		kind:    "jwt",
		re:      regexp.MustCompile(before + `eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
		needles: lits("eyJ"),
		keep:    1,
	},
	{
		// The header as typed, as a JSON or YAML pair, and as either of
		// those inside a JSON string.
		kind:   "bearer-token",
		re:     regexp.MustCompile(`(?i)(authorization(?:\\*["'])?\s*[:=]\s*(?:\\*["'])?bearer\s+)([A-Za-z0-9._~+/=-]{16,})`),
		keep:   1,
		secret: 2,
	},
	{
		// The 40-character secret beside an access key id, named as an env
		// var, an ini key, or the JSON field STS returns.
		kind:    "aws-secret-key",
		re:      regexp.MustCompile(`(?i)((?:aws[_-]?)?secret[_-]?access[_-]?key(?:\\*["'])?\s*[:=]\s*(?:\\*["'])?)([A-Za-z0-9/+]{40})`),
		needles: lits("ecret", "ECRET"),
		keep:    1,
		secret:  2,
	},
	{
		kind:    "aws-session-token",
		re:      regexp.MustCompile(`(?i)((?:aws[_-]?)?session[_-]?token(?:\\*["'])?\s*[:=]\s*(?:\\*["'])?)([A-Za-z0-9+/=]{20,})`),
		needles: lits("ession", "ESSION"),
		keep:    1,
		secret:  2,
	},
	{
		// A v2 session token with nothing naming it: the prefix is the
		// base64 the tokens themselves start with.
		kind:    "aws-session-token",
		re:      regexp.MustCompile(before + `IQoJb3JpZ2luX2Vj[A-Za-z0-9+/=]{30,}`),
		needles: lits("IQoJ"),
		keep:    1,
	},
	{
		kind:    "stripe-key",
		re:      regexp.MustCompile(before + `(?:sk|rk)_live_([A-Za-z0-9]{10,})`),
		needles: lits("sk_live_", "rk_live_"),
		keep:    1,
		secret:  2,
	},
	{
		kind:    "google-api-key",
		re:      regexp.MustCompile(before + `AIza[0-9A-Za-z_-]{35}\b`),
		needles: lits("AIza"),
		keep:    1,
	},
	{
		kind:    "gitlab-token",
		re:      regexp.MustCompile(before + `glpat-([A-Za-z0-9_-]{20,})`),
		needles: lits("glpat-"),
		keep:    1,
		secret:  2,
	},
	{
		kind:    "npm-token",
		re:      regexp.MustCompile(before + `npm_([A-Za-z0-9]{36})\b`),
		needles: lits("npm_"),
		keep:    1,
		secret:  2,
	},
	{
		// user:pass@ in a URL: the password goes, the rest of the URL stays.
		kind:    "url-password",
		re:      regexp.MustCompile(`([a-z][a-z0-9+.-]*://[^/\s:@"']+:)([^@/\s"']{3,})(@)`),
		needles: lits("://"),
		keep:    1,
		secret:  2,
		tail:    3,
	},
	{
		kind:   "api-key-header",
		re:     regexp.MustCompile(`(?i)(x-api-key(?:\\*["'])?\s*[:=]\s*(?:\\*["'])?)([A-Za-z0-9._~+/=-]{16,})`),
		keep:   1,
		secret: 2,
	},
	{
		// Authorization with a non-bearer scheme, or with a bare credential
		// and no scheme at all; the bearer rule above handles Bearer.
		kind:   "auth-header",
		re:     regexp.MustCompile(`(?i)(authorization(?:\\*["'])?\s*[:=]\s*(?:\\*["'])?(?:(?:basic|token|apikey)\s+)?)([A-Za-z0-9._~+/=-]{16,})`),
		keep:   1,
		secret: 2,
	},
	{
		// mysql's attached password: -p with no space is always a password.
		kind:    "mysql-password",
		re:      regexp.MustCompile(`(\bmysql\b[^\n;|&]*\s-p)([^\s"'\\;|&]+)`),
		needles: lits("mysql"),
		keep:    1,
	},
	{
		kind:    "curl-credential",
		re:      regexp.MustCompile(`(\bcurl\b[^\n;|&]*?\s(?:-u ?|--user[ =])(?:\\*["'])?[^\s:"'\\]{1,64}:)([^\s"'\\;|&]+)`),
		needles: lits("curl"),
		keep:    1,
	},
	{
		// PASSWORD, *_SECRET, and *_TOKEN assignments, as an env var or a
		// config pair. Last, so a vendor rule above names the kind first,
		// and its marker ([) stops this value match.
		kind:   "secret-assignment",
		re:     regexp.MustCompile(`(?i)((?:[A-Za-z0-9_]*password|[A-Za-z0-9_]+_(?:secret|token))(?:\\*["'])?\s*[:=]\s*(?:\\*["'])?)([A-Za-z0-9!._/+~@#%^*=-]{6,})`),
		keep:   1,
		secret: 2,
	},
}

func lits(s ...string) [][]byte {
	out := make([][]byte, len(s))
	for i, l := range s {
		out[i] = []byte(l)
	}
	return out
}

// generated reports whether b mixes digits with letters, as a generated
// token does and a hyphenated name or a placeholder like YOUR_API_KEY_HERE
// doesn't.
func generated(b []byte) bool {
	var digit, letter bool
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			letter = true
		}
	}
	return digit && letter
}

// Bytes returns b with every secret-shaped token replaced by
// [REDACTED:<kind>], and how many it replaced. With nothing to replace it
// returns b itself.
func Bytes(b []byte) ([]byte, int) {
	total := 0
	for _, r := range rules {
		var n int
		b, n = r.apply(b)
		total += n
	}
	return b, total
}

// String is Bytes for a string.
func String(s string) (string, int) {
	out, n := Bytes([]byte(s))
	if n == 0 {
		return s, 0
	}
	return string(out), n
}

func (r rule) apply(b []byte) ([]byte, int) {
	if !r.mayMatch(b) {
		return b, 0
	}
	group := func(loc []int, g int) []byte { return b[loc[2*g]:loc[2*g+1]] }
	var out []byte
	last, n := 0, 0
	for _, loc := range r.re.FindAllSubmatchIndex(b, -1) {
		if r.secret > 0 && !generated(group(loc, r.secret)) {
			continue
		}
		out = append(out, b[last:loc[0]]...)
		if r.keep > 0 {
			out = append(out, group(loc, r.keep)...)
		}
		out = append(append(append(out, "[REDACTED:"...), r.kind...), ']')
		if r.tail > 0 {
			out = append(out, group(loc, r.tail)...)
		}
		last = loc[1]
		n++
	}
	if n == 0 {
		return b, 0
	}
	return append(out, b[last:]...), n
}

func (r rule) mayMatch(b []byte) bool {
	if len(r.needles) == 0 {
		return true
	}
	for _, needle := range r.needles {
		if bytes.Contains(b, needle) {
			return true
		}
	}
	return false
}
