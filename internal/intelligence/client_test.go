package intelligence

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/config"
)

const testKey = "sk-ant-test-key-not-real-0123456789"

// fakeAPI is an httptest fake of Anthropic's Messages API and the OpenAI
// chat-completions protocol. Each request gets the next of the responses,
// the last one repeating.
type fakeAPI struct {
	srv       *httptest.Server
	mu        sync.Mutex
	responses []fakeResponse
	calls     int
	// path, header and body are the last request's.
	path   string
	header http.Header
	body   map[string]any
}

type fakeResponse struct {
	status int
	header map[string]string
	body   string
	// text is a 200's answer, when body is empty.
	text  string
	model string
	stop  string
}

func newFakeAPI(t *testing.T, responses ...fakeResponse) *fakeAPI {
	t.Helper()
	f := &fakeAPI{responses: responses}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		resp := f.responses[min(f.calls, len(f.responses)-1)]
		f.calls++
		b, _ := io.ReadAll(r.Body)
		f.path, f.header, f.body = r.URL.Path, r.Header.Clone(), map[string]any{}
		if r.Method != http.MethodPost || json.Unmarshal(b, &f.body) != nil {
			t.Errorf("fake API got %s %s %s", r.Method, r.URL.Path, b)
		}
		for k, v := range resp.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("content-type", "application/json")
		if resp.status != 0 {
			w.WriteHeader(resp.status)
		}
		if resp.body != "" {
			_, _ = io.WriteString(w, resp.body)
			return
		}
		var out any
		if strings.HasSuffix(r.URL.Path, "/v1/messages") {
			out = map[string]any{
				"id": "msg_1", "type": "message", "model": cmpOr(resp.model, "claude-opus-5-5"), "stop_reason": cmpOr(resp.stop, "end_turn"),
				"content": []map[string]any{{"type": "thinking", "thinking": "", "signature": "s"}, {"type": "text", "text": resp.text}},
				"usage":   map[string]int{"input_tokens": 1200, "output_tokens": 340, "cache_read_input_tokens": 3000, "cache_creation_input_tokens": 0},
			}
		} else {
			out = map[string]any{
				"id": "chatcmpl-1", "object": "chat.completion", "model": cmpOr(resp.model, "gpt-6.2-astra"),
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": resp.text}, "finish_reason": cmpOr(resp.stop, "stop")}},
				"usage":   map[string]any{"prompt_tokens": 4200, "completion_tokens": 340, "prompt_tokens_details": map[string]int{"cached_tokens": 3000}},
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client(t *testing.T, provider, model string) *Client {
	t.Helper()
	s, err := SettingsFrom(config.IntelligenceConfig{Enabled: true, Provider: provider, Model: model, BaseURL: f.srv.URL}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(s, testKey, WithUserAgent("tug/test"))
	c.backoff = func(int) time.Duration { return 0 }
	return c
}

func testEvidence() *Evidence {
	return &Evidence{
		Cluster: "payments-prod", Target: "pod/api-0", Namespace: "web", Collected: now,
		Items: []Item{
			{ID: "E1", Kind: KindDiagnosis, Source: "tug why pod/api-0", Text: "not ready: CrashLoopBackOff"},
			{ID: "E2", Kind: KindLog, Source: "log of pod/api-0 container api", Text: "panic: dial tcp db:5432: connect: connection refused"},
		},
	}
}

func answerJSON(a map[string]any) string {
	for _, k := range []string{"facts", "hypotheses", "nextSteps", "changesNeedingApproval", "instructionsInEvidence"} {
		if _, ok := a[k]; !ok {
			a[k] = []any{}
		}
	}
	for _, k := range []string{"summary", "likelyCause"} {
		if _, ok := a[k]; !ok {
			a[k] = ""
		}
	}
	b, _ := json.Marshal(a)
	return string(b)
}

func TestExplainWithClaude(t *testing.T) {
	f := newFakeAPI(t, fakeResponse{text: answerJSON(map[string]any{
		"summary":     "The container crashes because it cannot reach its database. Do not run `kubectl rollout restart deploy/api`; read `kubectl logs pod/api-0 -n web` instead.",
		"likelyCause": "The database at db:5432 refuses connections.",
		"facts": []map[string]any{
			{"statement": "The container is in CrashLoopBackOff.", "evidence": []string{"E1", "E9", "[E1]"}},
			{"statement": "The database runs in namespace data.", "evidence": []string{"E7"}},
		},
		"hypotheses": []map[string]any{
			{"statement": "The database Service has no endpoints.", "confidence": "medium", "evidence": []string{"E2"}, "wouldConfirm": "`kubectl get endpoints db -n web` showing none"},
		},
		"nextSteps": []map[string]any{
			{"description": "See whether the database has endpoints.", "command": "kubectl get endpoints db -n web"},
			{"description": "Restart it.", "command": "kubectl delete pod api-0 -n web"},
			{"description": "Read its password.", "command": "kubectl get secret db -n web -o yaml"},
		},
		"changesNeedingApproval": []map[string]any{
			{"description": "Point the app at the right host, if it is wrong: `kubectl set env deploy/api DB_HOST=db.data -n web`.", "command": "kubectl set env deploy/api DB_HOST=db.data -n web"},
			{"description": "Pipe it.", "command": "kubectl get pods | sh"},
		},
	})})
	ev := testEvidence()
	x, err := f.client(t, Anthropic, "").Explain(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}

	// The request: the stable guidance with a cache breakpoint, then this
	// call's evidence and the manner, then the ask; the answer's schema;
	// a fallback for a declined request.
	if f.path != "/v1/messages" {
		t.Errorf("request went to %s", f.path)
	}
	for k, want := range map[string]string{
		"X-Api-Key": testKey, "Anthropic-Version": anthropicVersion, "Anthropic-Beta": fallbackBeta,
		"Content-Type": "application/json", "User-Agent": "tug/test",
	} {
		if got := f.header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	b := f.body
	if b["model"] != "claude-opus-5-5" || b["max_tokens"] != float64(DefaultMaxOutputTokens) || b["fallbacks"] != "default" {
		t.Errorf("model, max_tokens, fallbacks = %v, %v, %v", b["model"], b["max_tokens"], b["fallbacks"])
	}
	for _, unwanted := range []string{"temperature", "top_p", "top_k", "thinking", "tools"} {
		if _, ok := b[unwanted]; ok {
			t.Errorf("request sets %s", unwanted)
		}
	}
	system := b["system"].([]any)
	stable, volatile := system[0].(map[string]any), system[1].(map[string]any)
	p := NewPrompt(ev)
	if stable["text"] != StableGuidance() || stable["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Errorf("first system block = %v, want the stable guidance with a cache breakpoint", stable)
	}
	if volatile["text"] != p.Volatile || volatile["cache_control"] != nil || !strings.HasSuffix(p.Volatile, Manner()) {
		t.Errorf("second system block = %v, want the volatile half ending with the manner", volatile)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != Ask() {
		t.Errorf("messages = %v, want the one ask", msgs)
	}
	oc := b["output_config"].(map[string]any)
	format := oc["format"].(map[string]any)
	if oc["effort"] != DefaultEffort || format["type"] != "json_schema" || format["schema"].(map[string]any)["additionalProperties"] != false {
		t.Errorf("output_config = %v", oc)
	}

	// The answer, as tug checked it, and the call.
	cost := (1200*4.0 + 340*20.0 + 3000*0.2) / 1e6
	want := &Explanation{
		Summary:     "The container crashes because it cannot reach its database. Do not run `[command removed by tug]`; read `kubectl logs pod/api-0 -n web` instead.",
		LikelyCause: "The database at db:5432 refuses connections.",
		Facts:       []Claim{{Statement: "The container is in CrashLoopBackOff.", Evidence: []string{"E1"}}},
		Hypotheses: []Claim{
			{Statement: "The database Service has no endpoints.", Evidence: []string{"E2"}, Confidence: "medium", WouldConfirm: "`kubectl get endpoints db -n web` showing none"},
			{Statement: "The database runs in namespace data.", Evidence: []string{}, Confidence: "low"},
		},
		NextSteps: []Step{
			{Description: "See whether the database has endpoints.", Command: "kubectl get endpoints db -n web"},
			{Description: "Read its password."},
		},
		ChangesNeedingApproval: []Step{
			{Description: "Restart it.", Command: "kubectl delete pod api-0 -n web"},
			{Description: "Point the app at the right host, if it is wrong: `kubectl set env deploy/api DB_HOST=db.data -n web`.", Command: "kubectl set env deploy/api DB_HOST=db.data -n web"},
			{Description: "Pipe it."},
		},
		InstructionsInEvidence: []Claim{},
		Removed: []string{
			"a command in the text that does not only read",
			"1 facts that cited no evidence tug sent, now shown as hypotheses",
			"a next step that changes something, moved to the changes needing approval",
			"a suggested command, because it would print a Secret's data",
			"a suggested change's command, because it uses the shell",
			"citations of evidence tug did not send: E7, E9",
		},
		Call: Call{
			Time: x.Call.Time, Provider: Anthropic, Model: "claude-opus-5-5", Target: "pod/api-0",
			Usage:   Usage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 3000},
			CostUSD: &cost, LatencyMS: x.Call.LatencyMS, RequestID: "msg_1",
		},
	}
	if !reflect.DeepEqual(x, want) {
		gotJSON, _ := json.MarshalIndent(x, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("Explain =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	if x.Call.LatencyMS < 0 || x.Call.Time.IsZero() {
		t.Errorf("call time %v, latency %dms", x.Call.Time, x.Call.LatencyMS)
	}
}

func TestExplainWithChatCompletions(t *testing.T) {
	tests := []struct {
		provider, model string
		maxField        string
		format          string
	}{
		{provider: OpenAI, model: "gpt-6.2-astra", maxField: "max_completion_tokens", format: "json_schema"},
		{provider: Mistral, model: "mistral-large-latest", maxField: "max_tokens", format: "json_schema"},
		{provider: Custom, model: "llama4:70b", maxField: "max_tokens", format: "json_object"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			f := newFakeAPI(t, fakeResponse{
				model:  tt.model,
				header: map[string]string{"x-request-id": "req_42"},
				text:   "```json\n" + answerJSON(map[string]any{"summary": "It cannot reach its database.", "facts": []map[string]any{{"statement": "It crashes.", "evidence": []string{"E1"}}}}) + "\n```",
			})
			ev := testEvidence()
			x, err := f.client(t, tt.provider, tt.model).Explain(context.Background(), ev)
			if err != nil {
				t.Fatal(err)
			}
			if f.path != "/chat/completions" || f.header.Get("authorization") != "Bearer "+testKey {
				t.Errorf("request to %s with authorization %q", f.path, f.header.Get("authorization"))
			}
			b := f.body
			msgs := b["messages"].([]any)
			system, user := msgs[0].(map[string]any), msgs[1].(map[string]any)
			p := NewPrompt(ev)
			if system["role"] != "system" || system["content"] != p.System() || !strings.HasPrefix(p.System(), StableGuidance()) || !strings.HasSuffix(p.System(), Manner()) {
				t.Errorf("system message does not carry the assembled guidance: %v", system["role"])
			}
			if user["role"] != "user" || user["content"] != Ask() {
				t.Errorf("user message = %v", user)
			}
			if b[tt.maxField] != float64(DefaultMaxOutputTokens) || b["model"] != tt.model {
				t.Errorf("request = model %v, %s %v", b["model"], tt.maxField, b[tt.maxField])
			}
			format := b["response_format"].(map[string]any)
			if format["type"] != tt.format {
				t.Errorf("response_format = %v, want %s", format, tt.format)
			}
			if tt.format == "json_schema" && format["json_schema"].(map[string]any)["strict"] != true {
				t.Errorf("json_schema is not strict: %v", format)
			}
			if x.Summary != "It cannot reach its database." || len(x.Facts) != 1 {
				t.Errorf("Explain = %+v", x)
			}
			// The prompt's count includes the cached tokens, which are
			// priced apart.
			if x.Call.Usage != (Usage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 3000}) || x.Call.RequestID != "req_42" || x.Call.Provider != tt.provider || x.Call.Model != tt.model {
				t.Errorf("call = %+v", x.Call)
			}
			if tt.provider == Custom && x.Call.CostUSD != nil {
				t.Errorf("a model without a price cost %v, want unknown", *x.Call.CostUSD)
			}
			if tt.provider == OpenAI {
				want := (1200*8.0 + 340*40.0 + 3000*0.4) / 1e6
				if x.Call.CostUSD == nil || *x.Call.CostUSD != want {
					t.Errorf("cost = %v, want %v", x.Call.CostUSD, want)
				}
			}
		})
	}
}

func TestACustomEndpointMayHaveNoKey(t *testing.T) {
	f := newFakeAPI(t, fakeResponse{text: answerJSON(map[string]any{"summary": "s"})})
	s, err := SettingsFrom(config.IntelligenceConfig{Provider: Custom, Model: "qwen3", BaseURL: f.srv.URL}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(s, "").Explain(context.Background(), testEvidence()); err != nil {
		t.Fatal(err)
	}
	if got := f.header.Get("authorization"); got != "" {
		t.Errorf("authorization = %q, want none", got)
	}
}

func TestExplainWithoutServerSideFallbacks(t *testing.T) {
	f := newFakeAPI(t, fakeResponse{text: answerJSON(map[string]any{"summary": "s"})})
	if _, err := f.client(t, Anthropic, "claude-haiku-4-5").Explain(context.Background(), testEvidence()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.body["fallbacks"]; ok || f.header.Get("anthropic-beta") != "" {
		t.Errorf("claude-haiku-4-5 request has fallbacks %v and beta %q", f.body["fallbacks"], f.header.Get("anthropic-beta"))
	}
	if effort := f.body["output_config"].(map[string]any)["effort"]; effort != nil {
		t.Errorf("claude-haiku-4-5 request has effort %v, which it rejects", effort)
	}
}

func TestExplainReadsTheAnswerAfterAFallback(t *testing.T) {
	f := newFakeAPI(t, fakeResponse{body: `{"model":"claude-opus-5","stop_reason":"end_turn","content":[
		{"type":"text","text":"{\"summary\":\"half an ans"},
		{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-opus-5"}},
		{"type":"text","text":` + jsonString(answerJSON(map[string]any{"summary": "from the fallback"})) + `}],
		"usage":{"input_tokens":10,"output_tokens":10}}`})
	x, err := f.client(t, Anthropic, "").Explain(context.Background(), testEvidence())
	if err != nil {
		t.Fatal(err)
	}
	// The call is priced as the model that answered.
	if x.Call.Model != "claude-opus-5" || x.Summary != "from the fallback" || x.Call.CostUSD == nil || *x.Call.CostUSD != (10*5.0+10*25.0)/1e6 {
		t.Errorf("Explain after a fallback = model %q, summary %q, cost %v", x.Call.Model, x.Summary, x.Call.CostUSD)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestExplainErrors(t *testing.T) {
	ok := fakeResponse{text: answerJSON(map[string]any{"summary": "s"})}
	overloaded := fakeResponse{status: 529, header: map[string]string{"retry-after": "0"},
		body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
	tests := []struct {
		name      string
		provider  string
		responses []fakeResponse
		calls     int
		check     func(*Explanation, error) bool
		want      string
	}{
		{
			name:      "a busy provider is tried again",
			responses: []fakeResponse{overloaded, ok},
			calls:     2,
			check:     func(_ *Explanation, err error) bool { return err == nil },
		},
		{
			name:      "but only twice",
			responses: []fakeResponse{overloaded},
			calls:     3,
			check: func(_ *Explanation, err error) bool {
				var e *APIError
				return errors.As(err, &e) && e.Status == 529
			},
			want: "anthropic answered 529 overloaded_error: Overloaded",
		},
		{
			name: "a rejected key is not, and the key stays out of the error",
			responses: []fakeResponse{{status: 401, header: map[string]string{"request-id": "req_1"},
				body: `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + testKey + `"}}`}},
			calls: 1,
			check: func(_ *Explanation, err error) bool {
				var e *APIError
				return errors.As(err, &e) && e.Unauthorized()
			},
			want: "anthropic answered 401 authentication_error: invalid x-api-key [key] (request req_1)",
		},
		{
			name:     "OpenAI's masked key stays out too",
			provider: OpenAI,
			responses: []fakeResponse{{status: 401, header: map[string]string{"x-request-id": "req_2"},
				body: `{"error":{"message":"Incorrect API key provided: sk-proj-****wxyz.","type":"invalid_request_error","code":"invalid_api_key"}}`}},
			calls: 1,
			check: func(_ *Explanation, err error) bool { return err != nil },
			want:  "openai answered 401 invalid_request_error: Incorrect API key provided: [key] (request req_2)",
		},
		{
			name:      "Mistral's error form",
			provider:  Mistral,
			responses: []fakeResponse{{status: 400, body: `{"object":"error","message":"Invalid model: mistral-huge","type":"invalid_model","code":"1500"}`}},
			calls:     1,
			check:     func(_ *Explanation, err error) bool { return err != nil },
			want:      "mistral answered 400 invalid_model: Invalid model: mistral-huge",
		},
		{
			name:      "nor what the provider says not to retry",
			responses: []fakeResponse{{status: 500, header: map[string]string{"x-should-retry": "false"}, body: `{}`}, ok},
			calls:     1,
			check:     func(_ *Explanation, err error) bool { return err != nil },
			want:      "anthropic answered 500",
		},
		{
			name:      "a refusal, which still costs",
			responses: []fakeResponse{{body: `{"model":"claude-opus-5-5","content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},"usage":{"input_tokens":100,"output_tokens":0}}`}},
			calls:     1,
			check: func(x *Explanation, err error) bool {
				return errors.Is(err, ErrRefused) && x != nil && x.Call.InputTokens == 100 && x.Call.CostUSD != nil
			},
			want: "the model declined to explain this evidence (cyber)",
		},
		{
			name:      "OpenAI's refusal",
			provider:  OpenAI,
			responses: []fakeResponse{{body: `{"model":"gpt-6.2-astra","choices":[{"message":{"content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`}},
			calls:     1,
			check:     func(x *Explanation, err error) bool { return errors.Is(err, ErrRefused) && x != nil },
			want:      "the model declined to explain this evidence",
		},
		{
			name:      "an answer cut short",
			responses: []fakeResponse{{stop: "max_tokens", text: `{"summ`}},
			calls:     1,
			check:     func(x *Explanation, err error) bool { return errors.Is(err, ErrTruncated) && x != nil },
			want:      "the model's answer was cut off before it was complete; raise intelligence.maxOutputTokens",
		},
		{
			name:      "an answer that is not the JSON asked for",
			responses: []fakeResponse{{text: `{"summary":"s","verdict":"x"}`}},
			calls:     1,
			check:     func(_ *Explanation, err error) bool { return err != nil },
			want:      `reading the model's answer: json: unknown field "verdict"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAPI(t, tt.responses...)
			x, err := f.client(t, cmpOr(tt.provider, Anthropic), "").Explain(context.Background(), testEvidence())
			if !tt.check(x, err) {
				t.Errorf("Explain = %+v, %v", x, err)
			}
			if err != nil && err.Error() != tt.want {
				t.Errorf("Explain error = %q, want %q", err, tt.want)
			}
			if f.calls != tt.calls {
				t.Errorf("Explain made %d requests, want %d", f.calls, tt.calls)
			}
		})
	}
}

func TestExplainStopsWhenCancelled(t *testing.T) {
	f := newFakeAPI(t, fakeResponse{status: 529, header: map[string]string{"retry-after": "30"}, body: `{}`})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.client(t, Anthropic, "").Explain(ctx, testEvidence()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Explain error = %v, want the context's", err)
	}
}

func TestSettingsFrom(t *testing.T) {
	defaults := func(provider string) Settings {
		return Settings{
			Provider: provider, Model: DefaultModel(provider), BaseURL: DefaultBaseURL(provider), Effort: DefaultEffort,
			Timeout: DefaultTimeout, MaxOutputTokens: DefaultMaxOutputTokens,
		}
	}
	tests := []struct {
		name string
		in   config.IntelligenceConfig
		o    Overrides
		want Settings
		err  string
	}{
		{name: "defaults", want: defaults(Anthropic)},
		{
			name: "chosen",
			in: config.IntelligenceConfig{Enabled: true, Provider: "OpenAI", Model: "gpt-6.2-sol", BaseURL: "https://llm.example.com/v1/",
				Timeout: config.Duration{Duration: time.Minute}, MaxOutputTokens: 4000, MonthlySpendCapUSD: 25},
			want: Settings{Enabled: true, Provider: OpenAI, Model: "gpt-6.2-sol", BaseURL: "https://llm.example.com/v1", Effort: DefaultEffort,
				Timeout: time.Minute, MaxOutputTokens: 4000, MonthlySpendCapUSD: 25},
		},
		{
			name: "another provider for one call takes its own address and model",
			in:   config.IntelligenceConfig{Provider: Anthropic, Model: "claude-sonnet-5-5", BaseURL: "https://gateway.example.com"},
			o:    Overrides{Provider: Mistral},
			want: defaults(Mistral),
		},
		{
			name: "another model for one call",
			in:   config.IntelligenceConfig{Provider: OpenAI},
			o:    Overrides{Model: "gpt-6-luna"},
			want: func() Settings { s := defaults(OpenAI); s.Model = "gpt-6-luna"; return s }(),
		},
		{
			name: "a custom endpoint on this machine may use http",
			in:   config.IntelligenceConfig{Provider: Custom, Model: "qwen3", BaseURL: "http://127.0.0.1:11434/v1"},
			want: Settings{Provider: Custom, Model: "qwen3", BaseURL: "http://127.0.0.1:11434/v1", Effort: DefaultEffort, Timeout: DefaultTimeout, MaxOutputTokens: DefaultMaxOutputTokens},
		},
		{name: "a custom endpoint needs a model", in: config.IntelligenceConfig{Provider: Custom, BaseURL: "http://localhost:11434/v1"}, err: "the custom provider has no default model; set intelligence.model or pass --model"},
		{name: "and an address", in: config.IntelligenceConfig{Provider: Custom, Model: "qwen3"}, err: "the custom provider needs intelligence.baseURL, the address of an OpenAI-compatible API such as http://localhost:11434/v1"},
		{name: "an unknown provider", in: config.IntelligenceConfig{Provider: "gemini"}, err: `intelligence.provider: "gemini" is not a provider; use one of anthropic, openai, mistral, custom`},
		{name: "an unknown provider for one call", o: Overrides{Provider: "gemini"}, err: `--provider: "gemini" is not a provider; use one of anthropic, openai, mistral, custom`},
		{name: "an unknown effort", in: config.IntelligenceConfig{Effort: "huge"}, err: `intelligence.effort "huge" is not one of low, medium, high, xhigh, max`},
		{name: "http elsewhere", in: config.IntelligenceConfig{BaseURL: "http://llm.example.com"}, err: `intelligence.baseURL "http://llm.example.com" must use https, so the API key is not sent in clear text`},
		{name: "a password in the URL", in: config.IntelligenceConfig{BaseURL: "https://u:p@llm.example.com"}, err: `intelligence.baseURL "https://u:p@llm.example.com" is not a URL such as https://api.openai.com/v1`},
		{name: "a negative cap", in: config.IntelligenceConfig{MonthlySpendCapUSD: -1}, err: "intelligence.monthlySpendCapUSD cannot be negative"},
		{name: "a price without a model", in: config.IntelligenceConfig{Prices: []config.ModelPrice{{Input: 1}}}, err: "intelligence.prices: a price has no model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SettingsFrom(tt.in, tt.o)
			if tt.err != "" {
				if err == nil || err.Error() != tt.err {
					t.Errorf("SettingsFrom(%+v, %+v) error = %v, want %q", tt.in, tt.o, err, tt.err)
				}
				return
			}
			got.Prices = Prices{}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SettingsFrom(%+v, %+v) = %+v, %v; want %+v", tt.in, tt.o, got, err, tt.want)
			}
		})
	}
}

func TestDefaultsAreTheDocumentedOnes(t *testing.T) {
	for provider, want := range map[string]string{Anthropic: "claude-opus-5-5", OpenAI: "gpt-6.2-astra", Mistral: "mistral-large-latest", Custom: ""} {
		if got := DefaultModel(provider); got != want {
			t.Errorf("DefaultModel(%s) = %q, want %q", provider, got, want)
		}
		if _, ok := (Prices{}).For(want); want != "" && !ok {
			t.Errorf("the default model %s has no price", want)
		}
	}
	if KeyEnv(OpenAI) != "TUG_SECRET_OPENAI_API_KEY" || !KeyRequired(Mistral) || KeyRequired(Custom) {
		t.Errorf("KeyEnv(openai) = %s", KeyEnv(OpenAI))
	}
}

// TestValidate checks each setting alone, so that a custom provider can be
// set before its model and address.
func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		in   config.IntelligenceConfig
		want string
	}{
		{in: config.IntelligenceConfig{}},
		{in: config.IntelligenceConfig{Provider: "Custom"}},
		{in: config.IntelligenceConfig{Provider: "gemini"}, want: `intelligence.provider: "gemini" is not a provider; use one of anthropic, openai, mistral, custom`},
		{in: config.IntelligenceConfig{Effort: "huge"}, want: `intelligence.effort "huge" is not one of low, medium, high, xhigh, max`},
		{in: config.IntelligenceConfig{BaseURL: "http://llm.example.com/"}, want: `intelligence.baseURL "http://llm.example.com" must use https, so the API key is not sent in clear text`},
		{in: config.IntelligenceConfig{BaseURL: "http://localhost:11434/v1"}},
		{in: config.IntelligenceConfig{Timeout: config.Duration{Duration: -time.Second}}, want: "intelligence.timeout cannot be negative"},
		{in: config.IntelligenceConfig{MaxOutputTokens: -1}, want: "intelligence.maxOutputTokens cannot be negative"},
		{in: config.IntelligenceConfig{MonthlySpendCapUSD: -1}, want: "intelligence.monthlySpendCapUSD cannot be negative"},
		{in: config.IntelligenceConfig{Prices: []config.ModelPrice{{Input: 1}}}, want: "intelligence.prices: a price has no model"},
	} {
		err := Validate(tt.in)
		if (tt.want == "" && err != nil) || (tt.want != "" && (err == nil || err.Error() != tt.want)) {
			t.Errorf("Validate(%+v) = %v, want %q", tt.in, err, tt.want)
		}
	}
}
