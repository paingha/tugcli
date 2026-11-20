package ssm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		in      string
		want    Config
		wantErr string
	}{
		{in: "ssm://i-0123456789abcdef0", want: Config{Target: "i-0123456789abcdef0"}},
		{in: "ssm://i-0123456789abcdef0?region=eu-west-1&profile=prod",
			want: Config{Target: "i-0123456789abcdef0", Region: "eu-west-1", Profile: "prod"}},
		{in: "ssm://I-0123456789ABCDEF0/", want: Config{Target: "i-0123456789abcdef0"}},
		{in: "ssm://i-01234567", want: Config{Target: "i-01234567"}},
		{in: "ssm://mi-0123456789abcdef0?profile=a%20b", want: Config{Target: "mi-0123456789abcdef0", Profile: "a b"}},
		{in: "https://i-0123456789abcdef0", wantErr: "not an ssm:// URL"},
		{in: "ssm:i-0123456789abcdef0", wantErr: "names no instance"},
		{in: "ssm://", wantErr: "names no instance"},
		{in: "ssm://web-1", wantErr: "not a managed instance ID"},
		{in: "ssm://i-0123456789abcdef0:22", wantErr: "more than an instance ID"},
		{in: "ssm://me@i-0123456789abcdef0", wantErr: "more than an instance ID"},
		{in: "ssm://i-0123456789abcdef0/x", wantErr: "more than an instance ID"},
		{in: "ssm://i-0123456789abcdef0#x", wantErr: "more than an instance ID"},
		{in: "ssm://i-0123456789abcdef0?zone=a", wantErr: `unknown parameter "zone"`},
		{in: "ssm://i-0123456789abcdef0?region=a-1&region=b-1", wantErr: "region more than once"},
		{in: "ssm://i-0123456789abcdef0?region=US_EAST", wantErr: "not an AWS region"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			u, err := url.Parse(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseURL(u)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseURL(%q) error = %v, want %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("ParseURL(%q) = %+v, %v, want %+v", tt.in, got, err, tt.want)
			}
		})
	}
	if _, err := ParseURL(nil); err == nil {
		t.Error("ParseURL(nil) error = nil")
	}
}

// isolateAWS points the AWS SDK at the given files and nothing else of the
// machine's.
func isolateAWS(t *testing.T, config, credentials string) {
	t.Helper()
	t.Setenv("AWS_CONFIG_FILE", config)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, key := range []string{
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_SSM", "AWS_CA_BUNDLE",
	} {
		t.Setenv(key, "")
	}
}

type apiCall struct {
	target, authorization string
	body                  map[string]any
}

// fakeSSMEndpoint serves the SSM JSON API's StartSession and
// TerminateSession, so Dial runs through the real SDK client.
func fakeSSMEndpoint(t *testing.T, streamURL string) (*httptest.Server, func() []apiCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []apiCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		call := apiCall{target: r.Header.Get("X-Amz-Target"), authorization: r.Header.Get("Authorization")}
		_ = json.Unmarshal(data, &call.body)
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch call.target {
		case "AmazonSSM.StartSession":
			fmt.Fprintf(w, `{"SessionId":%q,"StreamUrl":%q,"TokenValue":%q}`, testSessionID, streamURL, testToken)
		case "AmazonSSM.TerminateSession":
			fmt.Fprintf(w, `{"SessionId":%q}`, testSessionID)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"__type":"UnknownOperationException","message":"unknown operation"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []apiCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func TestDialThroughTheSSMAPI(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	credentials := filepath.Join(dir, "credentials")
	if err := os.WriteFile(config, []byte("[profile tunnel]\nregion = eu-west-1\n\n[profile bare]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := "[tunnel]\naws_access_key_id = AKIDTUGTEST\naws_secret_access_key = fake-secret\n\n" +
		"[bare]\naws_access_key_id = AKIDTUGTEST\naws_secret_access_key = fake-secret\n"
	if err := os.WriteFile(credentials, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateAWS(t, config, credentials)

	tests := []struct {
		name       string
		cfg        Config
		wantRegion string
		wantErr    string
	}{
		{name: "region from the profile", cfg: Config{Target: testTarget, Profile: "tunnel"}, wantRegion: "eu-west-1"},
		{name: "region from the URL", cfg: Config{Target: testTarget, Profile: "tunnel", Region: "us-west-2"}, wantRegion: "us-west-2"},
		{name: "no region", cfg: Config{Target: testTarget, Profile: "bare"}, wantErr: "?region="},
		{name: "unknown profile", cfg: Config{Target: testTarget, Profile: "missing"}, wantErr: `profile "missing"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeAgent{}
			streamURL, dialer := startAgent(t, agent)
			endpoint, calls := fakeSSMEndpoint(t, streamURL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			c, err := Dial(ctx, tt.cfg, "10.0.0.1:443", WithEndpoint(endpoint.URL), WithDialer(dialer))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Dial() error = %v, want it to mention %s", err, tt.wantErr)
				}
				if got := calls(); len(got) != 0 {
					t.Errorf("SSM API calls = %v before the configuration was usable", got)
				}
				return
			}
			if err != nil {
				agent.check(t)
				t.Fatalf("Dial() error = %v", err)
			}
			if _, err := c.Write([]byte("ping")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			buf := make([]byte, 4)
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
				t.Fatalf("Read() = %q, %v, want %q", buf, err, "ping")
			}
			if err := c.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			got := calls()
			if len(got) != 2 || got[0].target != "AmazonSSM.StartSession" || got[1].target != "AmazonSSM.TerminateSession" {
				t.Fatalf("SSM API calls = %+v, want StartSession then TerminateSession", got)
			}
			scope := "Credential=AKIDTUGTEST/"
			if a := got[0].authorization; !strings.Contains(a, scope) || !strings.Contains(a, "/"+tt.wantRegion+"/ssm/aws4_request") {
				t.Errorf("StartSession Authorization = %q, want a signature for ssm in %s", a, tt.wantRegion)
			}
			start := got[0].body
			params, _ := json.Marshal(start["Parameters"])
			if start["Target"] != testTarget || start["DocumentName"] != DocumentName ||
				string(params) != `{"host":["10.0.0.1"],"portNumber":["443"]}` {
				t.Errorf("StartSession body = %v", start)
			}
			if got[1].body["SessionId"] != testSessionID {
				t.Errorf("TerminateSession body = %v, want SessionId %s", got[1].body, testSessionID)
			}

			agent.mu.Lock()
			upgrade := agent.header.Clone()
			agent.mu.Unlock()
			if a := upgrade.Get("Authorization"); !strings.Contains(a, scope) || !strings.Contains(a, "/"+tt.wantRegion+"/ssmmessages/aws4_request") {
				t.Errorf("data channel Authorization = %q, want a signature for ssmmessages in %s", a, tt.wantRegion)
			}
			if upgrade.Get("X-Amz-Date") == "" {
				t.Error("data channel request has no X-Amz-Date")
			}
			agent.check(t)
		})
	}
}
