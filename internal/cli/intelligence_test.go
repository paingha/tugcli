package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/intelligence"
)

func TestIntelligenceStatus(t *testing.T) {
	home := isolate(t)
	t.Setenv("TUG_SECRET_ANTHROPIC_API_KEY", "")

	out := mustTug(t, "intelligence", "status")
	for _, want := range []string{
		"Enabled:     no; turn tug why --deep on with `tug intelligence enable`\n",
		"Provider:    anthropic\n",
		"Model:       claude-opus-5-5\n",
		"Endpoint:    https://api.anthropic.com/v1/messages\n",
		"API key:     none; run `tug intelligence key set anthropic`, or set TUG_SECRET_ANTHROPIC_API_KEY\n",
		"Price:       $4 in, $20 out, $0.2 cache read per million tokens\n",
		"This month:  $0.00 in 0 calls (UTC)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	writeConfig(t, home, `intelligence:
  enabled: true
  provider: custom
  model: llama4:70b
  baseURL: http://localhost:11434/v1
  monthlySpendCapUSD: 10
  prices:
    - {model: llama4, input: 0.2, output: 0.4}
`)
	var st intelligenceStatus
	if err := json.Unmarshal([]byte(mustTug(t, "intelligence", "status", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.Provider != "custom" || st.Endpoint != "http://localhost:11434/v1/chat/completions" || st.Key != "none needed" ||
		st.Price == nil || st.Price.Input != 0.2 || !st.Price.Configured || st.MonthlySpendCapUSD != 10 {
		t.Errorf("status -o json = %+v", st)
	}

	writeConfig(t, home, "intelligence:\n  provider: openai\n")
	t.Setenv("TUG_SECRET_OPENAI_API_KEY", "sk-openai-env-0123456789")
	out = mustTug(t, "intelligence", "status")
	if !strings.Contains(out, "API key:     from TUG_SECRET_OPENAI_API_KEY\n") || !strings.Contains(out, "Model:       gpt-6.2-astra\n") || strings.Contains(out, "sk-openai") {
		t.Errorf("status with a key in the environment:\n%s", out)
	}

	writeConfig(t, home, "intelligence:\n  provider: gemini\n")
	if r := tug(t, "intelligence", "status"); r.code != 1 || !strings.Contains(r.stderr, `intelligence.provider: "gemini" is not a provider`) {
		t.Errorf("status with an unknown provider: %+v", r)
	}
}

func TestIntelligenceKey(t *testing.T) {
	home := isolate(t)
	t.Setenv("TUG_SECRET_MISTRAL_API_KEY", "")

	// Keys are asked for on a terminal, never taken from an argument.
	r := tug(t, "intelligence", "key", "set", "mistral")
	if r.code != 1 || !strings.Contains(r.stderr, "tug intelligence key set asks for the key on a terminal; in scripts, set TUG_SECRET_MISTRAL_API_KEY instead") {
		t.Errorf("key set without a terminal: %+v", r)
	}
	if r := tug(t, "intelligence", "key", "set", "mistral", "sk-in-an-argument"); r.code != 1 || !strings.Contains(r.stderr, "accepts 1 arg(s), received 2") {
		t.Errorf("key set with the key as an argument: %+v", r)
	}
	if r := tug(t, "intelligence", "key", "set", "gemini"); r.code != 1 || !strings.Contains(r.stderr, `"gemini" is not a provider; use one of anthropic, openai, mistral, custom`) {
		t.Errorf("key set for an unknown provider: %+v", r)
	}

	// A stored key is found, shown only as where it is, and removed.
	const key = "mistral-key-from-the-terminal-0123456789"
	if err := fileStore(t).Set(credentials.IntelligenceKey("mistral"), key); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, home, "intelligence:\n  provider: mistral\n")
	if out := mustTug(t, "intelligence", "status"); !strings.Contains(out, "API key:     stored in ") || strings.Contains(out, key) {
		t.Errorf("status with a stored key:\n%s", out)
	}
	// The key lives only in the credential store: no other file under
	// tug's home holds it.
	mustTug(t, "intelligence", "usage")
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), key) {
			t.Errorf("%s holds the API key", path)
		}
		return nil
	})

	if out := mustTug(t, "intelligence", "key", "rm", "mistral"); !strings.HasPrefix(out, "Removed the mistral API key from ") {
		t.Errorf("key rm = %q", out)
	}
	if _, err := fileStore(t).Get(credentials.IntelligenceKey("mistral")); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("key after rm: %v", err)
	}
	if out := mustTug(t, "intelligence", "key", "rm", "mistral"); !strings.HasPrefix(out, "No mistral API key was stored in ") {
		t.Errorf("key rm of a missing key = %q", out)
	}
}

func TestIntelligenceModelsAndUsage(t *testing.T) {
	home := isolate(t)
	out := mustTug(t, "intelligence", "models")
	for _, want := range []string{
		"PROVIDER    DEFAULT MODEL                  ADDRESS\n",
		"anthropic   claude-opus-5-5                https://api.anthropic.com\n",
		"openai      gpt-6.2-astra                  https://api.openai.com/v1\n",
		"mistral     mistral-large-latest           https://api.mistral.ai/v1\n",
		"custom      none: set intelligence.model",
		"anthropic claude-opus-5-5 $4 $20 $0.2",
		"Prices are US dollars per million tokens, read from:\n",
		"  openai    https://developers.openai.com/api/docs/pricing (2026-11-21)\n",
	} {
		if !strings.Contains(squash(out), squash(want)) {
			t.Errorf("models lacks %q:\n%s", want, out)
		}
	}
	writeConfig(t, home, "intelligence:\n  prices:\n    - {model: llama4, input: 0.2, output: 0.4}\n")
	var m intelligenceModels
	if err := json.Unmarshal([]byte(mustTug(t, "intelligence", "models", "-o", "json")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Defaults["openai"] != "gpt-6.2-astra" || m.Defaults["custom"] != "" || m.Prices[0].Model != "llama4" || !m.Prices[0].Configured || len(m.Sources) != 3 {
		t.Errorf("models -o json = %+v", m)
	}

	if out := mustTug(t, "intelligence", "usage"); out != "No calls yet.\n" {
		t.Errorf("usage with no calls = %q", out)
	}
	paths := filepath.Join(home, "data")
	l := intelligence.OpenLedger(paths, nil)
	c := 0.0142
	if err := l.Record(intelligence.Call{Time: time.Now(), Provider: "anthropic", Model: "claude-opus-5-5", Target: "deploy/billing",
		Usage: intelligence.Usage{InputTokens: 2000, OutputTokens: 300, CacheReadTokens: 1000}, CostUSD: &c, LatencyMS: 4200, RequestID: "req_1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(intelligence.Call{Time: time.Now(), Provider: "custom", Model: "llama4", Usage: intelligence.Usage{InputTokens: 10, OutputTokens: 5}}); err != nil {
		t.Fatal(err)
	}
	out = mustTug(t, "intelligence", "usage")
	for _, want := range []string{
		"TIME                  PROVIDER    MODEL             TARGET           INPUT   OUTPUT   CACHED   COST      LATENCY   REQUEST\n",
		"custom      llama4            -                10      5        0        unknown   0.0s      -\n",
		"anthropic   claude-opus-5-5   deploy/billing   2,000   300      1,000    $0.0142   4.2s      req_1\n",
		"2 2,010 305 1,000 $0.0142 (+1 unpriced)",
	} {
		if !strings.Contains(squash(out), squash(want)) {
			t.Errorf("usage lacks %q:\n%s", want, out)
		}
	}
	if out := mustTug(t, "intelligence", "usage", "--limit", "1", "-o", "json"); strings.Count(out, `"requestId"`) != 0 || !strings.Contains(out, `"model": "llama4"`) {
		t.Errorf("usage --limit 1 lists more than the latest call:\n%s", out)
	}
}

// squash puts every run of spaces and newlines as one space, for checking a
// table's rows without its column widths.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// writeConfig writes config.yaml under an isolated home.
func writeConfig(t *testing.T, home, yaml string) {
	t.Helper()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fileStore opens the isolated home's credential store.
func fileStore(t *testing.T) credentials.Store {
	t.Helper()
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Open(credentials.BackendFile, paths)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestIntelligenceEnableAndDisable(t *testing.T) {
	home := isolate(t)
	for _, p := range intelligence.Providers {
		t.Setenv(intelligence.KeyEnv(p), "")
	}
	writeConfig(t, home, "# mine\nintelligence:\n  model: claude-sonnet-5-5\n  monthlySpendCapUSD: 20\n")

	// Without a terminal it cannot ask for the key, so it says how to add
	// one.
	out := mustTug(t, "intelligence", "enable")
	for _, want := range []string{
		"tug why --deep is on, asking anthropic's claude-sonnet-5-5 at https://api.anthropic.com/v1/messages.\n",
		"Store the anthropic API key with `tug intelligence key set anthropic`, or set TUG_SECRET_ANTHROPIC_API_KEY, before using it.\n",
		"Try it with `tug why <target> --deep`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("enable lacks %q:\n%s", want, out)
		}
	}
	if got, want := readConfig(t, home), "# mine\nintelligence:\n  model: claude-sonnet-5-5\n  monthlySpendCapUSD: 20\n  enabled: true\n  provider: anthropic\n"; got != want {
		t.Errorf("config.yaml after enable =\n%s\nwant\n%s", got, want)
	}

	// Another provider goes back to its own model, and a stored key is
	// found.
	if err := fileStore(t).Set(credentials.IntelligenceKey("openai"), "sk-openai-stored-0123456789"); err != nil {
		t.Fatal(err)
	}
	out = mustTug(t, "intelligence", "enable", "openai")
	if !strings.Contains(out, "asking openai's gpt-6.2-astra at https://api.openai.com/v1/chat/completions.\n") || !strings.Contains(out, "It uses the openai API key stored in ") || strings.Contains(out, "sk-openai") {
		t.Errorf("enable openai:\n%s", out)
	}
	if got := readConfig(t, home); strings.Contains(got, "claude-sonnet") || !strings.Contains(got, "provider: openai") || !strings.Contains(got, "monthlySpendCapUSD: 20") {
		t.Errorf("config.yaml after enable openai =\n%s", got)
	}

	// A custom endpoint needs a model and an address, and no key.
	if r := tug(t, "intelligence", "enable", "custom"); r.code != 1 || !strings.Contains(r.stderr, "the custom provider has no default model; set intelligence.model or pass --model") {
		t.Errorf("enable custom without a model: %+v", r)
	}
	if r := tug(t, "intelligence", "enable", "custom", "--model", "llama4:70b"); r.code != 1 || !strings.Contains(r.stderr, "needs intelligence.baseURL") || !strings.Contains(r.stderr, "; pass --base-url") {
		t.Errorf("enable custom without an address: %+v", r)
	}
	if r := tug(t, "intelligence", "enable", "custom", "--model", "llama4:70b", "--base-url", "http://llm.example.com/v1"); r.code != 1 || !strings.Contains(r.stderr, "must use https") {
		t.Errorf("enable custom with http elsewhere: %+v", r)
	}
	out = mustTug(t, "intelligence", "enable", "custom", "--model", "llama4:70b", "--base-url", "http://localhost:11434/v1")
	if !strings.Contains(out, "asking custom's llama4:70b at http://localhost:11434/v1/chat/completions.\n") || strings.Contains(out, "API key") {
		t.Errorf("enable custom:\n%s", out)
	}
	if r := tug(t, "intelligence", "enable", "gemini"); r.code != 1 || !strings.Contains(r.stderr, `"gemini" is not a provider`) {
		t.Errorf("enable gemini: %+v", r)
	}

	out = mustTug(t, "intelligence", "disable")
	if !strings.HasPrefix(out, "tug why --deep is off. Stored API keys are kept") {
		t.Errorf("disable = %q", out)
	}
	if got := readConfig(t, home); !strings.Contains(got, "enabled: false") || !strings.Contains(got, "model: llama4:70b") {
		t.Errorf("config.yaml after disable =\n%s", got)
	}
	if r := tug(t, "why", "billing", "--deep"); r.code != 1 || !strings.Contains(r.stderr, "--deep is off; turn it on with `tug intelligence enable`") {
		t.Errorf("--deep after disable: %+v", r)
	}
	if v, err := fileStore(t).Get(credentials.IntelligenceKey("openai")); err != nil || v == "" {
		t.Errorf("disable removed a stored key: %q, %v", v, err)
	}
}
