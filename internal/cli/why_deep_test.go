package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/intelligence"
)

// fakeModels is an httptest fake of Anthropic's Messages API and the
// OpenAI chat-completions protocol that answers every request with the
// same explanation, or with status when it is set.
type fakeModels struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status int
	// paths, keys and bodies are what each request sent: where to, its
	// API key, and its body.
	paths, keys []string
	bodies      []map[string]any
}

const fakeAnswer = `{
	"summary": "Its container keeps crashing on start-up; see the log.",
	"likelyCause": "The application exits as soon as it starts.",
	"facts": [{"statement": "Its container is in CrashLoopBackOff.", "evidence": ["E2"]}],
	"hypotheses": [{"statement": "The app exits as soon as it starts.", "confidence": "medium", "evidence": ["E2", "E40"], "wouldConfirm": "the previous run's log"}],
	"nextSteps": [
		{"description": "Read the log from before the restart.", "command": "kubectl logs pod/billing-5d4f-x2x4z --previous -n shop"},
		{"description": "Then restart it.", "command": "kubectl rollout restart deploy/billing -n shop"}
	],
	"changesNeedingApproval": [],
	"instructionsInEvidence": []
}`

func newFakeModels(t *testing.T) *fakeModels {
	t.Helper()
	f := &fakeModels{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("model request: %v %s", err, b)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Header.Get("x-api-key")
		if a := r.Header.Get("authorization"); a != "" {
			key = strings.TrimPrefix(a, "Bearer ")
		}
		f.paths, f.keys, f.bodies = append(f.paths, r.URL.Path), append(f.keys, key), append(f.bodies, body)
		w.Header().Set("content-type", "application/json")
		if f.status != 0 {
			w.WriteHeader(f.status)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}
		var out any
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("request-id", "req_claude")
			out = map[string]any{
				"model": "claude-opus-5-5", "stop_reason": "end_turn",
				"content": []map[string]string{{"type": "text", "text": fakeAnswer}},
				"usage":   map[string]int{"input_tokens": 2000, "output_tokens": 300, "cache_read_input_tokens": 1000},
			}
		} else {
			w.Header().Set("x-request-id", "req_gpt")
			out = map[string]any{
				"model":   body["model"],
				"choices": []map[string]any{{"message": map[string]any{"content": fakeAnswer}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": 2000, "completion_tokens": 300},
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModels) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

// system returns the request's system prompt, from either protocol.
func system(body map[string]any) string {
	if blocks, ok := body["system"].([]any); ok {
		var s []string
		for _, b := range blocks {
			s = append(s, b.(map[string]any)["text"].(string))
		}
		return strings.Join(s, "\n")
	}
	return body["messages"].([]any)[0].(map[string]any)["content"].(string)
}

// connected sets up an isolated home connected to the fake API server, with
// no model API key in the environment.
func connected(t *testing.T) (home string, srv *httptest.Server, writes func() int32) {
	t.Helper()
	home = isolate(t)
	s, w := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, s))
	newSession(t)
	mustTug(t, "connect", "payments-prod")
	for _, p := range intelligence.Providers {
		t.Setenv(intelligence.KeyEnv(p), "")
	}
	return home, s, w.Load
}

func TestWhyDeep(t *testing.T) {
	home, srv, writes := connected(t)
	models := newFakeModels(t)

	// Off unless config.yaml turns it on, and refused before the cluster is
	// read.
	r := tug(t, "why", "billing", "-n", "shop", "--deep", "--yes")
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "--deep is off; turn it on with `tug intelligence enable`") {
		t.Fatalf("--deep while off: %+v", r)
	}
	if r := tug(t, "why", "billing", "-n", "shop", "--model", "x"); r.code != 1 || !strings.Contains(r.stderr, "--provider and --model choose the model --deep asks; add --deep") {
		t.Errorf("--model without --deep: %+v", r)
	}
	writeConfig(t, home, "intelligence:\n  enabled: true\n  baseURL: "+models.srv.URL+"\n")

	// Without a key, it says how to store one, before reading the cluster.
	r = tug(t, "why", "billing", "-n", "shop", "--deep", "--yes")
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "tug: --deep needs an API key for anthropic; run `tug intelligence key set anthropic`, or set TUG_SECRET_ANTHROPIC_API_KEY") {
		t.Fatalf("--deep without a key: %+v", r)
	}
	t.Setenv("TUG_SECRET_ANTHROPIC_API_KEY", "sk-ant-from-env-0123456789")

	// Without a terminal and without --yes, it shows what it would send and
	// what it could cost, and refuses to send it.
	r = tug(t, "why", "billing", "-n", "shop", "--deep")
	if r.code != 1 || !strings.Contains(r.stdout, "deploy/billing in shop is not ready") ||
		!strings.Contains(r.stderr, "--deep will send this to anthropic, model claude-opus-5-5 at "+models.srv.URL+"/v1/messages, after tug's standing guidance and before its closing manner:") ||
		!strings.Contains(r.stderr, "# This request\n\nCluster: payments-prod\nNamespace: shop\nTarget: deploy/billing\nTime: ") ||
		!strings.Contains(r.stderr, "<evidence>\n[E1] diagnosis: tug why deploy/billing\n") ||
		!strings.Contains(r.stderr, " input tokens; estimated cost $") ||
		!strings.Contains(r.stderr, "This month (UTC): $0.00 spent.") ||
		!strings.Contains(r.stderr, "tug: --deep sends the evidence above to anthropic; without a terminal, pass --yes to agree") {
		t.Fatalf("--deep without a terminal or --yes: %+v", r)
	}
	if models.requests() != 0 {
		t.Fatalf("--deep sent %d requests without consent", models.requests())
	}

	// With --yes, it sends exactly what it showed, wrapped in the standing
	// guidance, and explains.
	r = tug(t, "why", "billing", "-n", "shop", "--deep", "--yes")
	if r.code != 1 {
		t.Fatalf("--deep --yes: exit %d, want 1 for a workload that is not ready\n%s%s", r.code, r.stdout, r.stderr)
	}
	if models.requests() != 1 || models.keys[0] != "sk-ant-from-env-0123456789" || models.paths[0] != "/v1/messages" {
		t.Fatalf("--deep --yes made %d requests (%q) with keys %q", models.requests(), models.paths, models.keys)
	}
	sent := system(models.bodies[0])
	if !strings.HasPrefix(sent, intelligence.StableGuidance()) || !strings.HasSuffix(sent, intelligence.Manner()) {
		t.Errorf("the request does not carry the standing guidance and the manner:\n%s", sent)
	}
	shown := r.stderr[strings.Index(r.stderr, "# This request"):strings.Index(r.stderr, "</evidence>\n")]
	if !strings.Contains(sent, shown) {
		t.Errorf("what was sent is not what was shown.\nshown:\n%s\nsent:\n%s", shown, sent)
	}
	for _, want := range []string{
		"[E2] finding: error about pod/billing-5d4f-x2x4z, container main\n    CrashLoopBackOff: keeps crashing and waits to restart; last exit code 1 (Error)\n",
		"] tree: tug tree deploy/billing: what it owns, with each part's health\n    deploy/billing [not-ready] ",
		"] log: log of pod/billing-5d4f-x2x4z container main, previous run, before its last restart, last line\n    log of billing-5d4f-x2x4z/main\n",
	} {
		if !strings.Contains(sent, want) {
			t.Errorf("sent evidence lacks %q:\n%s", want, sent)
		}
	}
	for _, want := range []string{
		"deploy/billing in shop is not ready: 0/1 ready\n",
		"\nExplanation by claude-opus-5-5 (AI-generated: check the evidence it cites)\nIts container keeps crashing on start-up; see the log.\nLikely cause: The application exits as soon as it starts.\n",
		"\nFacts:\n  - Its container is in CrashLoopBackOff. [E2]\n",
		"\nHypotheses:\n  - medium: The app exits as soon as it starts. [E2]\n    would confirm: the previous run's log\n",
		"\nNext steps (tug keeps only commands that read):\n  1. Read the log from before the restart.\n     kubectl logs pod/billing-5d4f-x2x4z --previous -n shop\n",
		"\nChanges that need your approval (tug runs none of them):\n  1. Then restart it.\n     kubectl rollout restart deploy/billing -n shop\n",
		"\nEvidence cited:\n  E2   finding: error about pod/billing-5d4f-x2x4z, container main\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("--deep output lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, want := range []string{
		"Asking claude-opus-5-5…\n",
		"anthropic claude-opus-5-5: 2,000 input + 1,000 from cache, 300 output tokens, $0.0142, ",
		", request req_claude\n",
		"warning: tug removed a next step that changes something, moved to the changes needing approval\n",
		"warning: tug removed citations of evidence tug did not send: E40\n",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("--deep stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stdout+r.stderr, "sk-ant-from-env") {
		t.Error("the API key appears in tug's output")
	}

	// JSON keeps the diagnosis's fields and adds what --deep sent, expected
	// and got.
	r = tug(t, "why", "billing", "-n", "shop", "--deep", "--yes", "-o", "json", "--provider", "openai", "--model", "gpt-6.2-sol")
	if r.code != 1 {
		t.Fatalf("--deep -o json without an OpenAI key: %+v", r)
	}
	if !strings.Contains(r.stderr, "--deep needs an API key for openai") {
		t.Errorf("--provider openai without its key: %s", r.stderr)
	}
	t.Setenv("TUG_SECRET_OPENAI_API_KEY", "sk-openai-from-env-0123456789")
	// --provider openai takes OpenAI's own address, not the configured
	// one, so point it at the fake through config.yaml.
	writeConfig(t, home, "intelligence:\n  enabled: true\n  provider: openai\n  baseURL: "+models.srv.URL+"/v1\n  monthlySpendCapUSD: 5\n")
	r = tug(t, "why", "billing", "-n", "shop", "--deep", "--yes", "-o", "json", "--model", "gpt-6.2-sol")
	var view struct {
		Target   string            `json:"target"`
		Findings []json.RawMessage `json:"findings"`
		Deep     struct {
			Sent     bool   `json:"sent"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Estimate struct {
				InputTokens int      `json:"inputTokens"`
				CostHighUSD *float64 `json:"costHighUSD"`
			} `json:"estimate"`
			Evidence struct {
				Cluster string `json:"cluster"`
				Items   []struct {
					ID string `json:"id"`
				} `json:"items"`
			} `json:"evidence"`
			Explanation struct {
				Summary                string `json:"summary"`
				ChangesNeedingApproval []struct {
					Command string `json:"command"`
				} `json:"changesNeedingApproval"`
				Call struct {
					Provider     string   `json:"provider"`
					Model        string   `json:"model"`
					InputTokens  int      `json:"inputTokens"`
					OutputTokens int      `json:"outputTokens"`
					CostUSD      *float64 `json:"costUSD"`
					LatencyMS    *int64   `json:"latencyMs"`
					RequestID    string   `json:"requestId"`
				} `json:"call"`
			} `json:"explanation"`
		} `json:"deep"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &view); err != nil || r.code != 1 {
		t.Fatalf("--deep -o json: exit %d, %v\n%s\n%s", r.code, err, r.stdout, r.stderr)
	}
	d, call := view.Deep, view.Deep.Explanation.Call
	if view.Target != "deploy/billing" || len(view.Findings) == 0 || !d.Sent || d.Provider != "openai" || d.Model != "gpt-6.2-sol" ||
		d.Estimate.InputTokens == 0 || d.Estimate.CostHighUSD == nil || d.Evidence.Cluster != "payments-prod" || len(d.Evidence.Items) < 5 ||
		d.Explanation.Summary == "" || len(d.Explanation.ChangesNeedingApproval) != 1 {
		t.Errorf("--deep -o json = %+v", view)
	}
	if call.Provider != "openai" || call.Model != "gpt-6.2-sol" || call.InputTokens != 2000 || call.OutputTokens != 300 ||
		call.CostUSD == nil || *call.CostUSD != (2000*2.0+300*10.0)/1e6 || call.LatencyMS == nil || call.RequestID != "req_gpt" {
		t.Errorf("--deep -o json call = %+v", call)
	}
	if last := models.paths[len(models.paths)-1]; last != "/v1/chat/completions" || models.keys[len(models.keys)-1] != "sk-openai-from-env-0123456789" {
		t.Errorf("OpenAI request went to %s", last)
	}

	// Nothing about the cluster's secrets or address is ever sent, and
	// nothing is written to it.
	for _, b := range models.bodies {
		sent, _ := json.Marshal(b)
		for _, secret := range []string{"hunter2", "not a real key", secretToken, srv.URL} {
			if strings.Contains(string(sent), secret) {
				t.Errorf("--deep sent %q", secret)
			}
		}
	}
	if writes() != 0 {
		t.Errorf("--deep made %d writes to the cluster", writes())
	}

	// Each call is in the ledger.
	var usage struct {
		Calls   []intelligence.Call  `json:"calls"`
		Monthly []intelligence.Total `json:"monthly"`
	}
	if err := json.Unmarshal([]byte(mustTug(t, "intelligence", "usage", "-o", "json")), &usage); err != nil {
		t.Fatal(err)
	}
	if len(usage.Calls) != 2 || usage.Calls[0].Provider != "openai" || usage.Calls[1].Provider != "anthropic" || usage.Calls[1].Target != "deploy/billing" ||
		len(usage.Monthly) != 1 || usage.Monthly[0].Calls != 2 {
		t.Errorf("usage after two calls = %+v", usage)
	}

	// A healthy target has nothing to explain.
	before := models.requests()
	r = tug(t, "why", "checkout", "-n", "shop", "--deep", "--yes")
	if r.code != 0 || !strings.Contains(r.stderr, "deploy/checkout is healthy, so --deep sent nothing.") || models.requests() != before {
		t.Errorf("--deep on a healthy target: %+v", r)
	}

	// --deep is for one object on one cluster.
	r = tug(t, "why", "billing", "-n", "shop", "--deep", "--yes", "--cluster-selector", "env=prod")
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "--deep explains one object on one cluster at a time; leave out --cluster-selector") || models.requests() != before {
		t.Errorf("--deep across clusters: %+v", r)
	}

	// The cap refuses a call that could pass it, before asking.
	writeConfig(t, home, "intelligence:\n  enabled: true\n  provider: openai\n  baseURL: "+models.srv.URL+"/v1\n  monthlySpendCapUSD: 0.05\n")
	r = tug(t, "why", "billing", "-n", "shop", "--deep", "--yes")
	if r.code != 1 || !strings.Contains(r.stderr, "tug: over the monthly spend cap: $") || !strings.Contains(r.stderr, "past the cap of $0.05; raise intelligence.monthlySpendCapUSD") || models.requests() != before {
		t.Errorf("--deep past the cap: %+v", r)
	}
	// And a model without a price, under a cap.
	r = tug(t, "why", "billing", "-n", "shop", "--deep", "--yes", "--model", "gpt-unknown")
	if r.code != 1 || !strings.Contains(r.stderr, "cost unknown, as gpt-unknown has no price (add one under intelligence.prices)") ||
		!strings.Contains(r.stderr, "this model has no price, so tug cannot tell what the call would cost") || models.requests() != before {
		t.Errorf("--deep with an unpriced model under a cap: %+v", r)
	}
}

// TestWhyDeepWithAStoredKey checks the key in the credential store, and
// what a rejected key says.
func TestWhyDeepWithAStoredKey(t *testing.T) {
	home, _, _ := connected(t)
	models := newFakeModels(t)
	writeConfig(t, home, "intelligence:\n  enabled: true\n  baseURL: "+models.srv.URL+"\n")
	store := fileStore(t)
	if err := store.Set(credentials.IntelligenceKey("anthropic"), "sk-ant-stored-0123456789"); err != nil {
		t.Fatal(err)
	}

	mustTugExit(t, 1, "why", "billing", "-n", "shop", "--deep", "--yes")
	if len(models.keys) != 1 || models.keys[0] != "sk-ant-stored-0123456789" {
		t.Fatalf("requests sent keys %q, want the stored one", models.keys)
	}

	models.status = http.StatusUnauthorized
	r := tug(t, "why", "billing", "-n", "shop", "--deep", "--yes")
	if r.code != 1 || !strings.Contains(r.stderr, "tug: anthropic rejected the stored API key; replace it with `tug intelligence key set anthropic`: anthropic answered 401 authentication_error: invalid x-api-key") {
		t.Fatalf("--deep with a rejected key: %+v", r)
	}
	// The key is kept: tug intelligence key set replaces it.
	if v, err := fileStore(t).Get(credentials.IntelligenceKey("anthropic")); err != nil || v != "sk-ant-stored-0123456789" {
		t.Errorf("stored key after a rejection = %q, %v", v, err)
	}
	if strings.Contains(r.stdout+r.stderr, "sk-ant-stored") {
		t.Error("the API key appears in tug's output")
	}
}

func mustTugExit(t *testing.T, code int, args ...string) result {
	t.Helper()
	r := tug(t, args...)
	if r.code != code {
		t.Fatalf("tug %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, code, r.stdout, r.stderr)
	}
	return r
}
