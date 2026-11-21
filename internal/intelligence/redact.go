package intelligence

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Placeholder kinds: what a placeholder stands for. A placeholder is the
// kind and a number, "[token 1]", and one value gets the same placeholder
// wherever it appears in a call.
const (
	KindPrivateKey    = "private key"
	KindAppliedConfig = "applied config"
	KindCredentials   = "url credentials"
	KindSignature     = "url signature"
	KindAuthorization = "authorization"
	KindToken         = "token"
	KindSecret        = "secret"
	KindEnv           = "env value"
)

// kindMeanings say what each kind of placeholder stands for, for the
// legend.
var kindMeanings = map[string]string{
	KindPrivateKey:    "a private key",
	KindAppliedConfig: "kubectl's last-applied copy of an object, which can hold its Secrets and environment",
	KindCredentials:   "the user name and password in a URL",
	KindSignature:     "a signed URL's signature or credential",
	KindAuthorization: "an Authorization header's credential",
	KindToken:         "a bearer token, JSON Web Token or API key",
	KindSecret:        "a value whose name says it is secret, such as a password",
	KindEnv:           "an environment variable's value",
}

// placeholderPattern matches a placeholder, so text that already has them
// is not redacted again.
var placeholderPattern = `\[(?:` + strings.Join(func() []string {
	var kinds []string
	for k := range kindMeanings {
		kinds = append(kinds, regexp.QuoteMeta(k))
	}
	slices.Sort(kinds)
	return kinds
}(), "|") + `) \d+\]`

var placeholderRe = regexp.MustCompile(placeholderPattern)

// HasPlaceholder reports whether s holds a placeholder.
func HasPlaceholder(s string) bool { return placeholderRe.MatchString(s) }

// value matches a value after a key: quoted, a placeholder, or up to the
// next space, quote or separator.
var value = `("[^"\n]*"|'[^'\n]*'|` + placeholderPattern + `|[^\s"',;&}\]]+)`

// secretKeyParts are the parts of a key, in any case, that mark its value
// as secret: DB_PASSWORD=x, "apiKey": "x", client-secret: x.
const secretKeyParts = `password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|signature|cookie|session[_-]?id`

// rule finds one kind of secret. Each match's group group is the secret,
// which is replaced by a placeholder; the rest of the match stays.
type rule struct {
	kind  string
	re    *regexp.Regexp
	group int
}

// rules are applied in order, so the broad ones at the end do not see what
// the specific ones have already replaced.
var rules = []rule{
	// A PEM private key, through to its END line or the end of the text.
	{KindPrivateKey, regexp.MustCompile(`(?s)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z))`), 1},
	// kubectl's copy of the last applied object, which holds whatever the
	// object held, Secret data and environment values included.
	{KindAppliedConfig, regexp.MustCompile(`kubectl\.kubernetes\.io/last-applied-configuration["']?\s*[:=]?\s*([^\n]*)`), 1},
	// The user and password in a URL: postgres://app:pw@db, redis://:pw@cache.
	{KindCredentials, regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://([^/\s@"'<>\[\]]+)@`), 1},
	// Signed URLs' query parameters.
	{KindSignature, regexp.MustCompile(`(?i)[?&](?:sig|key|code|x-amz-credential|x-amz-security-token|x-amz-signature|x-goog-credential|x-goog-signature)=([^&\s"'#\[\]]+)`), 1},
	// Authorization headers, with or without a scheme.
	{KindAuthorization, regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?\s*[:=]\s*["']?(?:(?:bearer|basic|digest|token|negotiate)\s+)?(` + placeholderPattern + `|[^\s"',;\[\]]+)`), 1},
	{KindToken, regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]+=*)`), 1},
	// JSON Web Tokens, which service account tokens are.
	{KindToken, regexp.MustCompile(`\b(eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*)`), 1},
	// Tokens and keys whose form gives them away: Anthropic, OpenAI-style,
	// GitHub, GitLab and Slack tokens, AWS access key IDs and Google API
	// keys.
	{KindToken, regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_-]{10,}|sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|` +
		`glpat-[A-Za-z0-9_-]{20,}|xox[abposr]-[A-Za-z0-9-]{10,}|(?:AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35})\b`), 1},
	// A value whose key says it is secret, in key=value, key: value and
	// JSON forms.
	{KindSecret, regexp.MustCompile(`(?i)\b[A-Za-z0-9_.-]*(?:` + secretKeyParts + `)[A-Za-z0-9_.-]*["']?\s*[:=]\s*` + value), 1},
	// An environment variable's value: FOO=bar, export DB_URL='x'.
	{KindEnv, regexp.MustCompile(`\b[A-Z][A-Z0-9_]*[A-Z0-9]=` + value), 1},
}

// Vault replaces secrets with placeholders for one call and remembers
// which kinds it used, for the legend. It never maps a placeholder back to
// its value: tug does not keep the values, and nothing it runs or shows
// gets them back.
type Vault struct {
	seen  map[string]string // kind and value to placeholder
	count map[string]int    // per kind
	n     int
}

// NewVault returns an empty vault.
func NewVault() *Vault { return &Vault{seen: map[string]string{}, count: map[string]int{}} }

// Redact replaces the secrets in s with placeholders: private keys, the
// last-applied annotation, credentials in URLs, signed URLs' signatures,
// authorization headers, bearer tokens and JWTs, tokens and keys of the
// common forms, values whose key names a secret, and the values of
// environment variables.
//
// It works on text alone, so it cannot know every secret; tug keeps the
// rest out by never putting Secrets' data, environment values or
// annotations in the evidence in the first place.
func (v *Vault) Redact(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllStringFunc(s, func(match string) string {
			loc := r.re.FindStringSubmatchIndex(match)
			start, end := loc[2*r.group], loc[2*r.group+1]
			if start < 0 {
				return match
			}
			secret := match[start:end]
			quote := ""
			if len(secret) >= 2 && (secret[0] == '"' || secret[0] == '\'') && secret[len(secret)-1] == secret[0] {
				quote, secret = secret[:1], secret[1:len(secret)-1]
			}
			if secret == "" || placeholderRe.MatchString(secret) {
				return match
			}
			return match[:start] + quote + v.placeholder(r.kind, secret) + quote + match[end:]
		})
	}
	return s
}

func (v *Vault) placeholder(kind, secret string) string {
	key := kind + "\x00" + secret
	if p, ok := v.seen[key]; ok {
		return p
	}
	v.count[kind]++
	v.n++
	p := fmt.Sprintf("[%s %d]", kind, v.count[kind])
	v.seen[key] = p
	return p
}

// Count says how many values were replaced.
func (v *Vault) Count() int { return v.n }

// Legend tells the model what the placeholders stand for, or is empty when
// nothing was redacted.
func (v *Vault) Legend() string {
	if v.n == 0 {
		return ""
	}
	kinds := make([]string, 0, len(v.count))
	for k := range v.count {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	var b strings.Builder
	b.WriteString("Some values in the evidence are placeholders: tug removed the real values before sending it, and keeps none of them.")
	for _, k := range kinds {
		fmt.Fprintf(&b, " [%s N] stands for %s.", k, kindMeanings[k])
	}
	b.WriteString(" A placeholder is not missing, a template or a mistake, so do not say it is, and do not guess the value behind it. The same placeholder means the same value wherever it appears. Never put a placeholder in a command.")
	return b.String()
}

// Redact replaces the secrets in s as a fresh Vault would and says how many
// it replaced.
func Redact(s string) (string, int) {
	v := NewVault()
	return v.Redact(s), v.Count()
}
