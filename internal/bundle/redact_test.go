package bundle

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRedactorText(t *testing.T) {
	r, err := NewRedactor([]string{`customer-[a-z]+-[0-9]+`})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, in, want string
		count          int
	}{
		{
			name:  "bearer token in a header",
			in:    "GET /v1/orders Authorization: Bearer abc123def456ghi789",
			want:  "GET /v1/orders Authorization: Bearer <redacted>",
			count: 1,
		},
		{
			name:  "basic credentials",
			in:    `upstream sent "authorization: Basic dXNlcjpodW50ZXIy"`,
			want:  `upstream sent "authorization: Basic <redacted>"`,
			count: 1,
		},
		{
			name:  "another scheme after Authorization",
			in:    "Authorization: Token tok_live_1234567",
			want:  "Authorization: Token <redacted>",
			count: 1,
		},
		{
			name:  "password in a URL",
			in:    "connecting to postgres://app:hunter2@db.shop:5432/orders?sslmode=require",
			want:  "connecting to postgres://app:<redacted>@db.shop:5432/orders?sslmode=require",
			count: 1,
		},
		{
			name:  "password in a URL without a user",
			in:    "redis://:s3cret@cache:6379/0",
			want:  "redis://:<redacted>@cache:6379/0",
			count: 1,
		},
		{
			name: "a URL without a password is kept",
			in:   "GET https://api.example.com:8443/v1/health 200",
			want: "GET https://api.example.com:8443/v1/health 200",
		},
		{
			name:  "JSON Web Token",
			in:    "session eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.c2lnbmF0dXJl expired",
			want:  "session <redacted> expired",
			count: 1,
		},
		{
			name:  "AWS access key ID",
			in:    "using key AKIAIOSFODNN7EXAMPLE for s3",
			want:  "using key <redacted> for s3",
			count: 1,
		},
		{
			name:  "GitHub and Slack tokens",
			in:    "gh=ghp_0123456789abcdefghijABCDEFGHIJ0123 slack xoxb-1234567890-abcdefghij",
			want:  "gh=<redacted> slack <redacted>",
			count: 2,
		},
		{
			name:  "key=value assignments",
			in:    "starting with DB_PASSWORD=hunter2 api_key=k-123 user=bob --client-secret=cs-9",
			want:  "starting with DB_PASSWORD=<redacted> api_key=<redacted> user=bob --client-secret=<redacted>",
			count: 3,
		},
		{
			name:  "JSON fields",
			in:    `{"level":"info","user":"bob","password":"hunter2","apiKey": "k-123","msg":"login"}`,
			want:  `{"level":"info","user":"bob","password":"<redacted>","apiKey": "<redacted>","msg":"login"}`,
			count: 2,
		},
		{
			name:  "YAML-like fields",
			in:    "config loaded: token: abc-123, secret: 'xyz'",
			want:  "config loaded: token: <redacted>, secret: '<redacted>'",
			count: 2,
		},
		{
			name: "an assignment does not reach the next line",
			in:   "password:\nnext line",
			want: "password:\nnext line",
		},
		{
			name: "words that are not assignments are kept",
			in:   `secret "pricing-api-key" not found; token expired`,
			want: `secret "pricing-api-key" not found; token expired`,
		},
		{
			name:  "PEM private key over several lines",
			in:    "key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nabc\n-----END RSA PRIVATE KEY-----\ndone",
			want:  "key:\n-----BEGIN RSA PRIVATE KEY-----<redacted>-----END RSA PRIVATE KEY-----\ndone",
			count: 1,
		},
		{
			name:  "a pattern from --redact-pattern",
			in:    "order for customer-acme-4242 shipped",
			want:  "order for <redacted> shipped",
			count: 1,
		},
		{
			name: "text already redacted is not counted again",
			in:   "password=<redacted> Bearer <redacted> postgres://app:<redacted>@db",
			want: "password=<redacted> Bearer <redacted> postgres://app:<redacted>@db",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n := r.Text(tt.in)
			if got != tt.want || n != tt.count {
				t.Errorf("Text(%q) = %q, %d\nwant %q, %d", tt.in, got, n, tt.want, tt.count)
			}
		})
	}
}

func TestNewRedactorRejectsBadPatterns(t *testing.T) {
	for _, p := range []string{`(unclosed`, `x*`} {
		if _, err := NewRedactor([]string{p}); err == nil {
			t.Errorf("NewRedactor(%q) = nil error, want one", p)
		}
	}
}

// decode turns JSON into the map an API server's object decodes to.
func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// marshal returns v as JSON without escaping the placeholder's brackets.
func marshal(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func TestRedactorObject(t *testing.T) {
	r, err := NewRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		gr   schema.GroupResource
		in   string
		// secrets must not be in the redacted object; kept must be.
		secrets, kept []string
		count         int
	}{
		{
			name: "env values in a pod, its init containers and its last-applied annotation",
			gr:   schema.GroupResource{Resource: "pods"},
			in: `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"api-0",
				"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"spec\":{\"containers\":[{\"env\":[{\"name\":\"DB_PASSWORD\",\"value\":\"env-hunter2\"}]}]}}"},
				"managedFields":[{"manager":"kubectl","fieldsV1":{"f:spec":{}}}]},
				"spec":{"initContainers":[{"name":"migrate","env":[{"name":"MIGRATE_TOKEN","value":"init-tok"}]}],
				"containers":[{"name":"app","image":"shop/api:v2",
					"args":["--listen=:8080","--api-token=arg-tok-9"],
					"env":[{"name":"DB_PASSWORD","value":"env-hunter2"},{"name":"LOG_LEVEL","value":"debug"},
						{"name":"DB_USER","valueFrom":{"secretKeyRef":{"name":"db","key":"user"}}}]}],
				"volumes":[{"name":"tls","secret":{"secretName":"api-tls"}}]}}`,
			secrets: []string{"env-hunter2", "init-tok", "arg-tok-9", "debug", "managedFields"},
			kept:    []string{"DB_PASSWORD", "LOG_LEVEL", "secretKeyRef", "api-tls", "--listen=:8080", "--api-token=<redacted>", "shop/api:v2"},
			count:   5,
		},
		{
			name: "env values in a deployment's pod template",
			gr:   schema.GroupResource{Group: "apps", Resource: "deployments"},
			in: `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},
				"spec":{"template":{"spec":{"containers":[{"name":"app","env":[{"name":"STRIPE_KEY","value":"sk_live_123"}]}]}}}}`,
			secrets: []string{"sk_live_123"},
			kept:    []string{"STRIPE_KEY"},
			count:   1,
		},
		{
			name: "ConfigMap data and binaryData values",
			gr:   schema.GroupResource{Resource: "configmaps"},
			in: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"api-config"},
				"data":{"DATABASE_URL":"postgres://app:cm-pass@db/orders","feature.flags":"beta=on","empty":""},
				"binaryData":{"cert.der":"YmluYXJ5"}}`,
			secrets: []string{"cm-pass", "beta=on", "YmluYXJ5"},
			kept:    []string{"DATABASE_URL", "feature.flags", "cert.der", `"empty":""`},
			count:   3,
		},
		{
			name: "a whole Secret keeps no data",
			gr:   schema.GroupResource{Resource: "secrets"},
			in: `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},
				"data":{"password":"aHVudGVyMg=="},"stringData":{"token":"plain-tok"}}`,
			secrets: []string{"aHVudGVyMg==", "plain-tok"},
			kept:    []string{`"name":"db"`},
			count:   2,
		},
		{
			name: "credential fields and parameters in a custom resource",
			gr:   schema.GroupResource{Group: "argoproj.io", Resource: "applications"},
			in: `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","metadata":{"name":"shop"},
				"spec":{"source":{"repoURL":"https://deploy:repo-pass@git.example.com/shop.git",
					"helm":{"parameters":[{"name":"db.password","value":"param-pass"},{"name":"replicas","value":"3"}],
						"values":"postgresql:\n  auth:\n    password: values-pass\n"}},
				"adminToken":"field-tok","tokenSecretName":"shop-token","credentialsPath":"/etc/creds"}}`,
			secrets: []string{"repo-pass", "param-pass", "values-pass", "field-tok"},
			kept:    []string{"git.example.com/shop.git", `"value":"3"`, "shop-token", "/etc/creds", "db.password"},
			count:   4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := decode(t, tt.in)
			n := r.Object(obj, tt.gr)
			got := marshal(t, obj)
			for _, s := range tt.secrets {
				if strings.Contains(got, s) {
					t.Errorf("Object kept %q:\n%s", s, got)
				}
			}
			for _, s := range tt.kept {
				if !strings.Contains(got, s) {
					t.Errorf("Object lost %q:\n%s", s, got)
				}
			}
			if n != tt.count {
				t.Errorf("Object redacted %d values, want %d:\n%s", n, tt.count, got)
			}
		})
	}
}

func TestRedactorValue(t *testing.T) {
	r, err := NewRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	v, n := r.Value([]any{"Bearer abcdefghijklmnop", map[string]any{"message": "dial postgres://u:pw@db"}})
	if got, want := marshal(t, v), `["Bearer <redacted>",{"message":"dial postgres://u:<redacted>@db"}]`; got != want || n != 2 {
		t.Errorf("Value = %s, %d; want %s, 2", got, n, want)
	}
	if s, n := r.Value("token=abc"); s != "token=<redacted>" || n != 1 {
		t.Errorf(`Value("token=abc") = %v, %d`, s, n)
	}
}
