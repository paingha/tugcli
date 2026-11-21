package intelligence

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		n    int
	}{
		{
			name: "environment variables",
			in:   `starting with DATABASE_URL=postgres://db:5432/app and export LOG_LEVEL='debug'`,
			want: `starting with DATABASE_URL=[env value 1] and export LOG_LEVEL='[env value 2]'`,
			n:    2,
		},
		{
			name: "a key that names a secret, in any case and form",
			in:   `password=hunter2 dbPassword: "two words" {"api_key":"abc123","client-secret": 'xyz'}`,
			want: `password=[secret 1] dbPassword: "[secret 2]" {"api_key":"[secret 3]","client-secret": '[secret 4]'}`,
			n:    4,
		},
		{
			name: "one value, one placeholder",
			in:   `password=hunter2 then PASSWORD: hunter2 again; token=other`,
			want: `password=[secret 1] then PASSWORD: [secret 1] again; token=[secret 2]`,
			n:    2,
		},
		{
			name: "credentials in a URL",
			in:   `dial postgres://app:s3cret@db.shop.svc:5432/orders failed; redis://:pw@cache:6379`,
			want: `dial postgres://[url credentials 1]@db.shop.svc:5432/orders failed; redis://[url credentials 2]@cache:6379`,
			n:    2,
		},
		{
			name: "a signed URL's query",
			in:   `GET https://bucket.s3.amazonaws.com/x?X-Amz-Credential=AKIDEXAMPLE&X-Amz-Signature=abcdef&part=1`,
			want: `GET https://bucket.s3.amazonaws.com/x?X-Amz-Credential=[url signature 1]&X-Amz-Signature=[url signature 2]&part=1`,
			n:    2,
		},
		{
			name: "authorization headers and bearer tokens",
			in:   `Authorization: Bearer abc.def.ghi; proxy-authorization=Basic dXNlcjpwYXNz; sent bearer xyz123`,
			want: `Authorization: Bearer [authorization 1]; proxy-authorization=Basic [authorization 2]; sent bearer [token 1]`,
			n:    3,
		},
		{
			name: "a JWT",
			in:   `token rejected: eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW0ifQ.c2lnbmF0dXJl`,
			want: `token rejected: [token 1]`,
			n:    1,
		},
		{
			name: "tokens and keys of known forms",
			in:   `ghp_0123456789abcdefghij0123 AKIAIOSFODNN7EXAMPLE sk-ant-api03-abcdefghijklmnop xoxb-1234567890-abc`,
			want: `[token 1] [token 2] [token 3] [token 4]`,
			n:    4,
		},
		{
			name: "a private key",
			in:   "loaded\n-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nIBAAKC\n-----END RSA PRIVATE KEY-----\ndone",
			want: "loaded\n[private key 1]\ndone",
			n:    1,
		},
		{
			name: "a private key cut short",
			in:   "-----BEGIN PRIVATE KEY-----\nMIIEow",
			want: "[private key 1]",
			n:    1,
		},
		{
			name: "the last-applied annotation",
			in:   `kubectl.kubernetes.io/last-applied-configuration: {"apiVersion":"v1","data":{"x":"y"}}` + "\nnext line",
			want: "kubectl.kubernetes.io/last-applied-configuration: [applied config 1]\nnext line",
			n:    1,
		},
		{
			name: "ordinary diagnostics stay as they are",
			in: `level=info msg="listening" addr=:8080; Back-off restarting failed container main; ` +
				`secret "pricing-api-key" not found; image sha256:0123456789abcdef0123456789abcdef; ` +
				`0/3 nodes are available: 1 Insufficient nvidia.com/gpu; Post "https://webhook.cert-manager.svc:443/validate?timeout=10s"`,
			want: `level=info msg="listening" addr=:8080; Back-off restarting failed container main; ` +
				`secret "pricing-api-key" not found; image sha256:0123456789abcdef0123456789abcdef; ` +
				`0/3 nodes are available: 1 Insufficient nvidia.com/gpu; Post "https://webhook.cert-manager.svc:443/validate?timeout=10s"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewVault()
			got := v.Redact(tt.in)
			if got != tt.want || v.Count() != tt.n {
				t.Errorf("Redact(%q) =\n%q, %d\nwant\n%q, %d", tt.in, got, v.Count(), tt.want, tt.n)
			}
			if again := v.Redact(got); again != got || v.Count() != tt.n {
				t.Errorf("Redact is not idempotent: Redact(%q) = %q, count %d", got, again, v.Count())
			}
		})
	}
}

// TestRedactLeavesNoSecret runs every secret in the fixtures through Redact
// inside a log line and checks that none of it is left.
func TestRedactLeavesNoSecret(t *testing.T) {
	secrets := []string{
		"hunter2", "s3cret", "dXNlcjpwYXNz", "AKIAIOSFODNN7EXAMPLE",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig_nature",
	}
	lines := []string{
		`{"level":"error","msg":"auth failed","password":"hunter2"}`,
		`connecting to mysql://root:s3cret@mysql:3306`,
		`Authorization: Basic dXNlcjpwYXNz`,
		`AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`,
		`using token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig_nature`,
	}
	for _, line := range lines {
		got, _ := Redact(line)
		for _, s := range secrets {
			if strings.Contains(got, s) {
				t.Errorf("Redact(%q) = %q, which still holds %q", line, got, s)
			}
		}
	}
}

func TestLegend(t *testing.T) {
	v := NewVault()
	if v.Legend() != "" {
		t.Errorf("Legend with nothing redacted = %q, want empty", v.Legend())
	}
	v.Redact(`LOG_LEVEL=debug and Bearer abcdef`)
	legend := v.Legend()
	for _, want := range []string{
		"tug removed the real values before sending it, and keeps none of them.",
		"[env value N] stands for an environment variable's value.",
		"[token N] stands for a bearer token, JSON Web Token or API key.",
		"do not guess the value behind it.",
		"Never put a placeholder in a command.",
	} {
		if !strings.Contains(legend, want) {
			t.Errorf("Legend lacks %q:\n%s", want, legend)
		}
	}
	if strings.Contains(legend, "secret N") {
		t.Errorf("Legend explains a kind that was not used:\n%s", legend)
	}
}
