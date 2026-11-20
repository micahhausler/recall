package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// The fake secrets are assembled at run time, so no file in the repo holds
// a token-shaped string for a secret scanner to trip on.
var (
	awsKey    = "AK" + "IA" + strings.Repeat("Q7", 8)
	githubTok = "gh" + "p_" + strings.Repeat("a1B2", 9)
	githubPAT = "github" + "_pat_" + strings.Repeat("a1B2_", 6)
	slackTok  = "xo" + "xb-" + "1234567890-" + strings.Repeat("aB3", 6)
	apiKey    = "s" + "k-" + strings.Repeat("a1B2c3", 8)
	antKey    = "s" + "k-ant-" + "api03-" + strings.Repeat("Zz9_", 10)
	jwt       = "ey" + "J" + strings.Repeat("hbGci", 4) + "." + "ey" + "J" + strings.Repeat("zdWIi", 4) + "." + strings.Repeat("sIg9", 6)
	bearerTok = "abc123" + "def456" + "ghi789"
	pemHead   = "-----BEGIN " + "RSA PRIVATE KEY-----"
	pemTail   = "-----END " + "RSA PRIVATE KEY-----"
	pemLines  = []string{pemHead, strings.Repeat("MIIEow", 10), strings.Repeat("q9+/Zz", 10) + "==", pemTail}
)

// More run-time-assembled fakes, with digits mixed in where a rule demands a
// generated-looking value, and "EXAMPLE"-style bodies everywhere else.
var (
	awsSecret  = strings.Repeat("Wx9y", 10)
	awsSession = strings.Repeat("Fw9x", 10)
	awsV2Tok   = "IQ" + "oJb3JpZ2luX2Vj" + strings.Repeat("EH7a", 15)
	stripeKey  = "sk" + "_live_" + strings.Repeat("a1B2", 6)
	stripeRK   = "rk" + "_live_" + strings.Repeat("c3D4", 6)
	googleKey  = "AI" + "za" + "SyA1" + strings.Repeat("bcD", 10) + "9"
	gitlabTok  = "gl" + "pat-" + strings.Repeat("x9Yz", 6)
	npmTok     = "np" + "m_" + strings.Repeat("a9Bc", 9)
	urlPass    = "p4ss" + "w0rdZ"
	headerKey  = "Xy7" + strings.Repeat("k2Lm", 5)
	basicCred  = strings.Repeat("Qx4z", 5) + "=="
	mysqlPass  = "s3" + "cr3tpw"
	curlPass   = "hu" + "nter42x"
	envSecret  = "s3" + "cr3t" + "X9!"
)

func TestWidenedKindsAreReplaced(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"aws secret key", "aws_secret_access_key = " + awsSecret, "aws_secret_access_key = [REDACTED:aws-secret-key]"},
		{"aws secret key in json", `"SecretAccessKey": "` + awsSecret + `"`, `"SecretAccessKey": "[REDACTED:aws-secret-key]"`},
		{"aws session token", "export AWS_SESSION_TOKEN=" + awsSession, "export AWS_SESSION_TOKEN=[REDACTED:aws-session-token]"},
		{"aws session token bare", "token " + awsV2Tok + " expires", "token [REDACTED:aws-session-token] expires"},
		{"stripe live key", "key=" + stripeKey, "key=[REDACTED:stripe-key]"},
		{"stripe restricted key", "use " + stripeRK + " here", "use [REDACTED:stripe-key] here"},
		{"google api key", "curl https://maps.example.com?key=" + googleKey, "curl https://maps.example.com?key=[REDACTED:google-api-key]"},
		{"gitlab token", "PRIVATE-TOKEN: " + gitlabTok, "PRIVATE-TOKEN: [REDACTED:gitlab-token]"},
		{"npm token", "published with " + npmTok, "published with [REDACTED:npm-token]"},
		{"url password", "git clone https://you:" + urlPass + "@example.com/repo.git", "git clone https://you:[REDACTED:url-password]@example.com/repo.git"},
		{"x-api-key header", "cu" + "rl -H 'x-api-key: " + headerKey + "' https://example.com", "cu" + "rl -H 'x-api-key: [REDACTED:api-key-header]' https://example.com"},
		{"authorization header", "Authorization: Basic " + basicCred, "Authorization: Basic [REDACTED:auth-header]"},
		{"mysql password flag", "mysql -h db.example.com -u root -p" + mysqlPass + " app", "mysql -h db.example.com -u root -p[REDACTED:mysql-password] app"},
		{"user flag credentials", "cu" + "rl -u you:" + curlPass + " https://example.com", "cu" + "rl -u you:[REDACTED:curl-credential] https://example.com"},
		{"password assignment", "export DB_PASSWORD=" + envSecret, "export DB_PASSWORD=[REDACTED:secret-assignment]"},
		{"secret assignment", "CLIENT_SECRET: " + envSecret, "CLIENT_SECRET: [REDACTED:secret-assignment]"},
		{"token assignment", `API_TOKEN="` + envSecret + `"`, `API_TOKEN="[REDACTED:secret-assignment]"`},
	} {
		got, n := String(tc.in)
		if got != tc.want || n != 1 {
			t.Errorf("%s: String = %q (%d), want %q (1)", tc.name, got, n, tc.want)
		}
	}
}

func TestWidenedLookalikesAreLeftAlone(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"aws secret key prose", "the aws secret access key lives in the credentials file"},
		{"aws secret key placeholder", "aws_secret_access_key = " + strings.Repeat("abcd", 10)},
		{"aws session token prose", "IQoJ marks a v2 session token; rotate the session token hourly"},
		{"stripe prefix prose", "a sk_live_ key must never land in a repo"},
		{"stripe placeholder", "sk" + "_live_" + "placeholdervalue"},
		{"google prefix prose", "AIza is how a Google API key starts"},
		{"gitlab placeholder", "gl" + "pat-" + "your-token-goes-here"},
		{"npm env var", "npm_config_registry=https://registry.example.com"},
		{"url with port", "https://example.com:8080/path is the endpoint"},
		{"url with user only", "https://you@example.com/repo.git"},
		{"x-api-key prose", "pass the x-api-key header on every request"},
		{"authorization prose", "Authorization headers are stripped by the proxy"},
		{"authorization placeholder", "Authorization: Basic YOUR_CREDENTIALS_HERE"},
		{"mysql prompt", "mysql -u root -p mydb"},
		{"curl without password", "curl -u you https://example.com"},
		{"token count", "MAX_TOKEN_COUNT=4096"},
		{"password placeholder", "PASSWORD=changeme"},
		{"variable reference", "GITHUB_TOKEN=$CI_TOKEN"},
	} {
		if got, n := String(tc.in); got != tc.in || n != 0 {
			t.Errorf("%s: String(%q) = %q (%d), want it unchanged", tc.name, tc.in, got, n)
		}
	}
}

func TestEachKindIsReplaced(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"aws", "export AWS_ACCESS_KEY_ID=" + awsKey + " now", "export AWS_ACCESS_KEY_ID=[REDACTED:aws-access-key] now"},
		{"github", "token " + githubTok + ".", "token [REDACTED:github-token]."},
		{"github pat", githubPAT, "[REDACTED:github-token]"},
		{"slack", "SLACK=" + slackTok, "SLACK=[REDACTED:slack-token]"},
		{"api key", "key: " + apiKey, "key: [REDACTED:api-key]"},
		{"prefixed api key", "x-api-key: " + antKey, "x-api-key: [REDACTED:api-key]"},
		{"jwt", "id_token=" + jwt + "&x=1", "id_token=[REDACTED:jwt]&x=1"},
		{"pem", "key:\n" + strings.Join(pemLines, "\n") + "\ndone", "key:\n[REDACTED:private-key]\ndone"},
		{"bearer", "Authorization: Bearer " + bearerTok, "Authorization: Bearer [REDACTED:bearer-token]"},
		{"bearer in curl", `curl -H "authorization: bearer ` + bearerTok + `" x`, `curl -H "authorization: bearer [REDACTED:bearer-token]" x`},
		{"bearer as a json pair", `{"Authorization": "Bearer ` + bearerTok + `"}`, `{"Authorization": "Bearer [REDACTED:bearer-token]"}`},
	} {
		got, n := String(tc.in)
		if got != tc.want || n != 1 {
			t.Errorf("%s: String = %q (%d), want %q (1)", tc.name, got, n, tc.want)
		}
	}
}

func TestLookalikesAreLeftAlone(t *testing.T) {
	for _, in := range []string{
		"",
		"nothing secret here",
		"the task-force-for-everything-and-more met",        // sk- inside a word
		"class sk-spinner-container-wrapper-large",          // sk- with no digits
		"Authorization: Bearer $GITHUB_TOKEN_FROM_THE_ENV",  // a variable, not a token
		"Authorization: Bearer YOUR_ACCESS_TOKEN_GOES_HERE", // a placeholder
		"Authorization: Bearer <token>",
		"AKIA is how an access key id starts",
		"see -----BEGIN PRIVATE KEY----- blocks, which end with a matching line",
		"ghp_short",
		"eyJhbGciOiJIUzI1NiJ9 alone is one base64 segment",
	} {
		if got, n := String(in); got != in || n != 0 {
			t.Errorf("String(%q) = %q (%d), want it unchanged", in, got, n)
		}
	}
}

func TestCountsEveryReplacement(t *testing.T) {
	in := githubTok + " and " + githubTok + "\n" + awsKey + "\nAuthorization: Bearer " + jwt
	got, n := String(in)
	if n != 4 {
		t.Errorf("replaced %d, want 4: %q", n, got)
	}
	for _, secret := range []string{githubTok, awsKey, jwt} {
		if strings.Contains(got, secret) {
			t.Errorf("a secret survived: %q", got)
		}
	}
}

// A transcript line is one JSON object, with every newline inside a string
// escaped; a token can sit right after such an escape.
func TestJSONLStaysJSONL(t *testing.T) {
	type record struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Tail string `json:"tail"`
	}
	inner, err := json.Marshal(map[string]string{"pem": strings.Join(pemLines, "\n"), "Authorization": "Bearer " + jwt})
	if err != nil {
		t.Fatal(err)
	}
	recs := []record{
		{"user", "here is the key\n" + strings.Join(pemLines, "\n") + "\nuse it", "after"},
		{"tool_result", "line one\n" + githubTok + "\r\n" + awsKey + "\t" + slackTok + "\n" + apiKey, "after"},
		{"tool_result", string(inner), "json inside json: " + string(inner)},
		{"assistant", "a block starts with " + pemHead, "after"},
		{"assistant", "and ends with " + pemTail + " \"quoted\" \\ backslash", "after"},
	}
	var in []byte
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		in = append(append(in, line...), '\n')
	}
	out, n := Bytes(in)
	// 1 PEM; 4 tokens; a PEM and a bearer's JWT at each of two nestings.
	if n != 9 {
		t.Errorf("replaced %d, want 9:\n%s", n, out)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(recs) {
		t.Fatalf("%d lines out, %d in:\n%s", len(lines), len(recs), out)
	}
	for i, line := range lines {
		var got record
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is no longer JSON: %v\n%s", i, err, line)
		}
		if got.Type != recs[i].Type {
			t.Errorf("line %d: type = %q, want %q", i, got.Type, recs[i].Type)
		}
	}
	for _, secret := range []string{githubTok, awsKey, slackTok, apiKey, jwt, pemLines[1], pemLines[2]} {
		if strings.Contains(string(out), secret) {
			t.Errorf("a secret survived:\n%s", out)
		}
	}
	// The two halves of a PEM block in different records are prose about
	// keys, not a key: nothing between them is swallowed.
	var a, b record
	json.Unmarshal([]byte(lines[3]), &a)
	json.Unmarshal([]byte(lines[4]), &b)
	if a != recs[3] || b != recs[4] {
		t.Errorf("records that only mention a PEM line changed: %+v %+v", a, b)
	}
	var first record
	json.Unmarshal([]byte(lines[0]), &first)
	if first.Text != "here is the key\n[REDACTED:private-key]\nuse it" || first.Tail != "after" {
		t.Errorf("first record = %+v", first)
	}
}

func TestUnchangedInputIsReturnedAsIs(t *testing.T) {
	in := []byte("plain text")
	out, n := Bytes(in)
	if n != 0 || &out[0] != &in[0] {
		t.Errorf("Bytes copied an input it didn't change (n=%d)", n)
	}
}
