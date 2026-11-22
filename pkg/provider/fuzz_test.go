package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fuzzProvider implements every operation and answers from the request, so
// that fuzzed requests reach each part of the SDK's encoding.
type fuzzProvider struct{}

var (
	_ AccountLister = fuzzProvider{}
	_ Rotator       = fuzzProvider{}
	_ AccessChecker = fuzzProvider{}
)

func (fuzzProvider) Info() Info {
	return Info{Name: "fuzz", Version: "0.0.1", Secrets: []SecretSpec{{Name: "token"}}}
}

func (fuzzProvider) Accounts(context.Context) ([]Account, error) {
	return []Account{{ID: "default"}}, nil
}

func (fuzzProvider) List(_ context.Context, req ListRequest) ([]Cluster, error) {
	if req.Account.ID == "" {
		return nil, Errorf(InvalidConfig, "no account").WithHint("pass one")
	}
	return []Cluster{{ID: req.Account.ID, Name: req.Account.ConfigString("name"), Labels: map[string]string{"k": req.Secrets["token"]}}}, nil
}

func (fuzzProvider) Credential(_ context.Context, req CredentialRequest) (Credential, error) {
	switch req.ClusterID {
	case "":
		return Credential{}, Errorf(NotFound, "no cluster")
	case "invalid":
		return Credential{Type: CredentialDelegate}, nil
	}
	return Token("t-"+req.ClusterID, time.Time{}), nil
}

func (p fuzzProvider) Rotate(ctx context.Context, req CredentialRequest) (Credential, error) {
	return p.Credential(ctx, req)
}

func (fuzzProvider) CheckAccess(_ context.Context, req AccessRequest) ([]Finding, error) {
	return []Finding{{Severity: SeverityInfo, Message: req.EgressIP}}, nil
}

// fuzzOps are the operations a one-shot fuzz input may run, chosen by its
// first byte.
var fuzzOps = []string{OpDescribe, OpAccounts, OpList, OpCredential, OpRotate, OpCheckAccess}

// decodeAny decodes a response to op with op's decoder.
func decodeAny(op string, data []byte) error {
	var err error
	switch op {
	case OpDescribe:
		_, err = DecodeDescribe(data)
	case OpAccounts:
		_, err = DecodeAccounts(data)
	case OpList:
		_, err = DecodeList(data)
	case OpCredential, OpRotate:
		_, err = DecodeCredential(data)
	case OpCheckAccess:
		_, err = DecodeCheckAccess(data)
	default:
		_, err = DecodeList(data)
	}
	return err
}

// seedRequests are well-formed and nearly well-formed requests, the seed
// corpus for the request fuzzers.
func seedRequests(t testing.TB) map[string][][]byte {
	t.Helper()
	acct := Account{ID: "a", Config: map[string]any{"name": "one", "n": 1.5}}
	payloads := map[string]any{
		OpDescribe:    nil,
		OpAccounts:    nil,
		OpList:        ListRequest{Account: acct, Secrets: map[string]string{"token": "s"}},
		OpCredential:  CredentialRequest{Account: acct, ClusterID: "c-1"},
		OpRotate:      CredentialRequest{Account: acct, ClusterID: "invalid"},
		OpCheckAccess: AccessRequest{Account: acct, Cluster: Cluster{ID: "c-1", CAData: []byte{0, 1}}, EgressIP: "192.0.2.1"},
	}
	out := map[string][][]byte{}
	for op, payload := range payloads {
		b, err := EncodeRequest(op, payload)
		if err != nil {
			t.Fatal(err)
		}
		out[op] = append(out[op], b,
			nil,
			[]byte("{}"),
			[]byte(`{"apiVersion":"provider/v2","kind":"ListRequest"}`),
			[]byte(`{"kind":"Error"}`),
			[]byte(`{"account":{"id":7}}`),
			[]byte(`[1,2,3]`),
			b[:len(b)/2],
		)
	}
	return out
}

// FuzzRun feeds one-shot requests to Run and checks the plugin side of the
// protocol: whatever arrives on stdin, stdout carries exactly one JSON
// message, which the host can decode, and the exit code agrees with it.
func FuzzRun(f *testing.F) {
	for op, seeds := range seedRequests(f) {
		i := slices.Index(fuzzOps, op)
		for _, s := range seeds {
			f.Add(byte(i), s)
		}
	}
	f.Fuzz(func(t *testing.T, pick byte, stdin []byte) {
		op := fuzzOps[int(pick)%len(fuzzOps)]
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), fuzzProvider{}, []string{op}, bytes.NewReader(stdin), &stdout, &stderr)
		out := stdout.Bytes()
		if bytes.Count(out, []byte("\n")) != 1 || !bytes.HasSuffix(out, []byte("\n")) {
			t.Fatalf("Run(%s) wrote %q, want one line of JSON", op, out)
		}
		err := decodeAny(op, out)
		var pe *Error
		switch {
		case code == ExitOK && err != nil:
			t.Fatalf("Run(%s) exited 0 but its response does not decode: %v\n%s", op, err, out)
		case code != ExitOK && !errors.As(err, &pe):
			t.Fatalf("Run(%s) exited %d without an Error message: %v\n%s", op, code, err, out)
		case pe != nil && pe.Code == "":
			t.Fatalf("Run(%s) answered an Error without a code: %s", op, out)
		}
	})
}

// FuzzServe feeds lines to serve mode and checks that each line is
// answered as the protocol says: a request with an id gets one response
// with that id, a line that is not a request gets one with no id, cancel
// and blank lines get none, and every response decodes.
func FuzzServe(f *testing.F) {
	for op, seeds := range seedRequests(f) {
		for _, s := range seeds {
			line, err := json.Marshal(ServeRequest{ID: "1", Op: op, Request: validRaw(s)})
			if err != nil {
				f.Fatal(err)
			}
			f.Add(string(line) + "\n" + `{"id":"1","cancel":true}` + "\n")
		}
	}
	f.Add(`{"id":"a","op":"describe"}` + "\n\n" + `not json` + "\n" + `{"id":"","op":"list"}` + "\n" + `{"id":"b","op":"serve"}`)
	f.Add(`{"id":"x","op":"list","request":null}` + "\r\n" + `{"id":"y","op":"credential","request":"str"}`)
	f.Fuzz(func(t *testing.T, in string) {
		want := map[string]int{}
		var ops []string
		for _, line := range strings.Split(in, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			var req ServeRequest
			switch {
			case json.Unmarshal([]byte(line), &req) != nil || req.ID == "":
				want[""]++
			case req.Cancel:
			default:
				want[req.ID]++
				ops = append(ops, req.Op)
			}
		}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), fuzzProvider{}, []string{OpServe}, strings.NewReader(in), &stdout, &stderr)
		if code != ExitOK {
			t.Fatalf("serve exited %d: %s", code, stderr.String())
		}
		got := map[string]int{}
		sc := bufio.NewScanner(&stdout)
		sc.Buffer(nil, maxServeLine)
		for sc.Scan() {
			var resp ServeResponse
			if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
				t.Fatalf("serve wrote a line that is not a response: %q", sc.Text())
			}
			got[resp.ID]++
			var tm typeMeta
			if err := json.Unmarshal(resp.Response, &tm); err != nil || tm.APIVersion != APIVersion || tm.Kind == "" {
				t.Fatalf("response %q has no apiVersion and kind: %s", resp.ID, resp.Response)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("serve answered %v, want %v (ops %v)", got, want, ops)
		}
		for id, n := range want {
			if got[id] != n {
				t.Fatalf("serve answered %q %d times, want %d (all: %v)", id, got[id], n, got)
			}
		}
	})
}

// validRaw returns b as a RawMessage when it is JSON, and nil otherwise,
// since a ServeRequest cannot carry anything else.
func validRaw(b []byte) json.RawMessage {
	if json.Valid(b) {
		return b
	}
	return nil
}

// FuzzDecode feeds plugin responses to every decoder, as the host reads
// them, and checks what comes back: errors are *Error with a code,
// credentials are valid, and a suggested wait is neither negative nor
// longer than a day.
func FuzzDecode(f *testing.F) {
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, msg := range []any{
		describeResponseMsg{typeMeta: meta(kindDescribeResponse), Description: Description{Name: "x", Protocols: SupportedAPIVersions(), Capabilities: CapList}},
		accountsResponseMsg{typeMeta: meta(kindAccountsResponse), Accounts: []Account{{ID: "a"}}},
		listResponseMsg{typeMeta: meta(kindListResponse), Clusters: []Cluster{{ID: "c", Credential: &Credential{Type: CredentialToken, Token: "t"}}}},
		credentialResponseMsg{typeMeta: meta(kindCredentialResponse), Credential: Credential{Type: CredentialClientCert, CertData: []byte("c"), KeyData: []byte("k"), ExpiresAt: &exp}},
		credentialResponseMsg{typeMeta: meta(kindCredentialResponse), Credential: Credential{Type: CredentialDelegate, Exec: &ExecConfig{Command: "x", ProvideClusterInfo: true, ClusterConfig: json.RawMessage(`{"a":1}`)}}},
		checkAccessResponseMsg{typeMeta: meta(kindCheckAccessResponse), Findings: []Finding{{Severity: SeverityError, Message: "m"}}},
		errorResponseMsg{typeMeta: meta(kindError), Error: Error{Code: RateLimited, Message: "slow", RetryAfterSeconds: 30}},
	} {
		b, err := json.Marshal(msg)
		if err != nil {
			f.Fatal(err)
		}
		for i := range fuzzOps {
			f.Add(byte(i), b)
		}
	}
	f.Add(byte(0), []byte(`{"kind":"Error","code":"RateLimited","retryAfterSeconds":9223372036854775807}`))
	f.Add(byte(0), []byte(`{"kind":"Error","code":"RateLimited","retryAfterSeconds":-30}`))
	f.Add(byte(3), []byte(`{"apiVersion":"provider/v1alpha1","kind":"CredentialResponse","credential":{"type":"delegate","exec":{"command":"x","clusterConfig":"{"}}}`))
	f.Add(byte(2), []byte(`{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[{"caData":"!!"}]}`))
	f.Fuzz(func(t *testing.T, pick byte, data []byte) {
		op := fuzzOps[int(pick)%len(fuzzOps)]
		err := decodeAny(op, data)
		if err == nil {
			if op == OpCredential || op == OpRotate {
				c, _ := DecodeCredential(data)
				if verr := c.Validate(); verr != nil {
					t.Fatalf("DecodeCredential accepted an invalid credential: %v", verr)
				}
			}
			return
		}
		var pe *Error
		if !errors.As(err, &pe) {
			t.Fatalf("decoding %s gave %T, want *Error: %v", op, err, err)
		}
		if pe.Code == "" {
			t.Fatalf("decoding %s gave an error without a code: %+v", op, pe)
		}
		if d := pe.RetryAfter(); d < 0 || d > maxRetryAfter {
			t.Fatalf("RetryAfter() = %s for retryAfterSeconds %d, want 0 to %s", d, pe.RetryAfterSeconds, maxRetryAfter)
		}
	})
}
