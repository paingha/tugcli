package bundle

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Placeholder replaces every value the bundle leaves out. It is a plain
// scalar in YAML and needs no escaping in JSON, so a redacted file still
// parses.
const Placeholder = "<redacted>"

// lastApplied is the annotation in which kubectl apply keeps a copy of the
// object as applied, a Secret's data included.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// textRule is one pattern that finds credentials in free text.
type textRule struct {
	re *regexp.Regexp
	// repl is the replacement, with $1-style references to what the
	// pattern keeps around the credential.
	repl string
}

// textRules find credentials in logs, events and every string value of an
// object, in this order: those that keep a prefix, such as "Bearer ", come
// before the general key=value rule, so it does not swallow the prefix.
var textRules = []textRule{
	// PEM private keys, which span lines.
	{regexp.MustCompile(`(?s)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----).*?(-----END [A-Z0-9 ]*PRIVATE KEY-----)`), "${1}" + Placeholder + "${2}"},
	// Bearer and basic credentials, as in an Authorization header.
	{regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)[A-Za-z0-9._~+/=-]{8,}`), "${1}${2}" + Placeholder},
	// Passwords in URLs: scheme://user:password@host.
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s:/?#@"']*):[^\s/?#@"']+@`), "${1}:" + Placeholder + "@"},
	// JSON Web Tokens: three base64url parts, the first a JSON header.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]+`), Placeholder},
	// AWS access key IDs.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), Placeholder},
	// GitHub, GitLab and Slack tokens, which have prefixes of their own.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abposr]-[A-Za-z0-9-]{10,})`), Placeholder},
	// Assignments to a key that names a credential, on one line:
	// password=..., --api-token=..., "apiKey": "...", Authorization: Token ....
	{regexp.MustCompile(`(?i)(["']?[\w.-]*` + credentialWords + `[\w.-]*["']?[ \t]*[:=][ \t]*["']?` +
		`(?:(?:bearer|basic|token|digest)[ \t]+)?)[^\s"',;&}\]]+`), "${1}" + Placeholder},
}

// credentialWords are the words that, in a key or field name, say its value
// is a credential.
const credentialWords = `(?:passw(?:or)?d|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?|authorization)`

// credentialField matches the names of object fields whose value is a
// credential, such as password, adminToken or apiKey.
var credentialField = regexp.MustCompile(`(?i)` + credentialWords)

// referenceSuffixes end the names of fields that name or point to a
// credential rather than hold one, such as secretName or tokenPath.
var referenceSuffixes = []string{"name", "names", "ref", "path", "file", "namespace", "kind", "type", "mode", "selector", "id", "uid", "url", "endpoint", "seconds", "audience"}

// Redactor removes credentials from what the bundle collects. It is safe for
// concurrent use once made.
type Redactor struct {
	extra []*regexp.Regexp
}

// NewRedactor returns a Redactor that also removes whatever the patterns,
// regular expressions in Go's syntax, match.
func NewRedactor(patterns []string) (*Redactor, error) {
	r := &Redactor{}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redact pattern %q: %w", p, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("redact pattern %q matches empty text; it would redact nothing useful", p)
		}
		r.extra = append(r.extra, re)
	}
	return r, nil
}

// Patterns returns how many patterns the Redactor was given. The bundle
// records the count, never the patterns, which may spell out what they hide.
func (r *Redactor) Patterns() int { return len(r.extra) }

// Text returns s with every credential the rules and patterns find
// replaced, and how many credentials it replaced.
func (r *Redactor) Text(s string) (string, int) {
	n := 0
	for _, rule := range textRules {
		var c int
		s, c = replace(rule.re, s, rule.repl)
		n += c
	}
	for _, re := range r.extra {
		var c int
		s, c = replace(re, s, Placeholder)
		n += c
	}
	return s, n
}

// replace replaces each match of re in s with the expanded template, and
// counts the matches it changed: a rule that finds text it already redacted
// leaves it as it is.
func replace(re *regexp.Regexp, s, tmpl string) (string, int) {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s, 0
	}
	var b strings.Builder
	last, n := 0, 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		out := string(re.ExpandString(nil, tmpl, s, m))
		if out != s[m[0]:m[1]] {
			n++
		}
		b.WriteString(out)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), n
}

// Object redacts an object as the API server returned it, in place, and
// returns how many values it replaced: the data of a ConfigMap, the values
// of environment variables, fields that hold a credential, the
// last-applied annotation, and what the text rules find in every other
// string. It drops managedFields, which only say which client set which
// field. gr is the object's group and resource.
func (r *Redactor) Object(obj map[string]any, gr schema.GroupResource) int {
	n := 0
	if md, ok := obj["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			if _, ok := ann[lastApplied]; ok {
				ann[lastApplied] = Placeholder
				n++
			}
		}
	}
	switch gr {
	case schema.GroupResource{Resource: "configmaps"}:
		for _, field := range []string{"data", "binaryData"} {
			if data, ok := obj[field].(map[string]any); ok {
				for k, v := range data {
					if v != "" && v != Placeholder {
						data[k] = Placeholder
						n++
					}
				}
			}
		}
	case schema.GroupResource{Resource: "secrets"}:
		// Secrets are read as metadata only; should one ever arrive whole,
		// none of its data is kept.
		for _, field := range []string{"data", "stringData"} {
			if _, ok := obj[field]; ok {
				delete(obj, field)
				n++
			}
		}
	}
	return n + r.walk(obj)
}

// Value redacts the strings anywhere in a decoded JSON value, as Object
// does apart from what is particular to ConfigMaps and Secrets, and returns
// the result and how many values it replaced. Maps and slices are changed
// in place.
func (r *Redactor) Value(v any) (any, int) {
	switch v := v.(type) {
	case string:
		return r.Text(v)
	case map[string]any:
		return v, r.walk(v)
	case []any:
		return v, r.walkList(v, false)
	default:
		return v, 0
	}
}

// walk redacts a map's values: string fields named like a credential, and
// what the text rules find in every other string.
func (r *Redactor) walk(m map[string]any) int {
	n := 0
	for k, v := range m {
		switch v := v.(type) {
		case string:
			if v == "" || v == Placeholder {
				continue
			}
			if holdsCredential(k) {
				m[k] = Placeholder
				n++
				continue
			}
			var c int
			m[k], c = r.Text(v)
			n += c
		case map[string]any:
			n += r.walk(v)
		case []any:
			n += r.walkList(v, k == "env")
		}
	}
	return n
}

// walkList redacts a list's items. In a container's env list, every
// variable's value goes; in any other list of name and value pairs, such as
// Helm parameters, the values of those named like a credential do. Names and
// valueFrom references stay: they say where a value comes from without
// holding it.
func (r *Redactor) walkList(list []any, env bool) int {
	n := 0
	for i, item := range list {
		switch item := item.(type) {
		case map[string]any:
			name, named := item["name"].(string)
			if v, ok := item["value"].(string); ok && named && v != "" && v != Placeholder && (env || holdsCredential(name)) {
				item["value"] = Placeholder
				n++
			}
			n += r.walk(item)
		case []any:
			n += r.walkList(item, false)
		case string:
			var c int
			list[i], c = r.Text(item)
			n += c
		}
	}
	return n
}

// holdsCredential reports whether a field's name says that its string value
// is a credential, rather than the name of or a path to one.
func holdsCredential(field string) bool {
	if !credentialField.MatchString(field) {
		return false
	}
	lower := strings.ToLower(field)
	for _, s := range referenceSuffixes {
		if strings.HasSuffix(lower, s) {
			return false
		}
	}
	return true
}
