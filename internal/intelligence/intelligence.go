// Package intelligence is tug's single door to model providers: nothing else
// in tug talks to one. It backs `tug why --deep`, which explains a diagnosis
// with a model the user chooses: Anthropic's Claude, OpenAI, Mistral or any
// OpenAI-compatible endpoint.
//
// It collects bounded evidence by reading only, redacts it into placeholders
// with a legend, and wraps it in tug's standing guidance: a stable half that
// reads the same on every call, a volatile half with this call's evidence,
// and a closing manner. The answer separates facts, each citing the
// evidence, from hypotheses; suggested commands that do more than read are
// listed apart, as needing a person's approval, and tug runs none of them.
// It prices every call from an embedded table, keeps a local usage ledger
// and enforces an optional monthly spend cap. It neither prints nor prompts.
package intelligence

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/paingha/tugcli/internal/config"
)

// The providers.
const (
	// Anthropic is Claude, through Anthropic's Messages API.
	Anthropic = "anthropic"
	// OpenAI and Mistral speak OpenAI's chat-completions protocol.
	OpenAI  = "openai"
	Mistral = "mistral"
	// Custom is any other endpoint that speaks it: Ollama, vLLM, LiteLLM,
	// an Azure OpenAI-style gateway.
	Custom = "custom"
)

// Providers are the providers tug can ask, in the order it lists them.
var Providers = []string{Anthropic, OpenAI, Mistral, Custom}

// providerDefaults are each provider's API address and default model, from
// the providers' own documentation (see prices.yaml for where and when).
// Mistral's documentation names no API id for its current large model, so
// its default is the -latest alias Mistral's API has long served.
var providerDefaults = map[string]struct{ baseURL, model string }{
	Anthropic: {"https://api.anthropic.com", "claude-opus-5-5"},
	OpenAI:    {"https://api.openai.com/v1", "gpt-6.2-astra"},
	Mistral:   {"https://api.mistral.ai/v1", "mistral-large-latest"},
	Custom:    {},
}

// DefaultModel returns a provider's default model, or "" for custom, which
// has none.
func DefaultModel(provider string) string { return providerDefaults[provider].model }

// DefaultBaseURL returns a provider's API address, or "" for custom.
func DefaultBaseURL(provider string) string { return providerDefaults[provider].baseURL }

// Defaults for the settings config.yaml leaves out.
const (
	DefaultProvider        = Anthropic
	DefaultTimeout         = 5 * time.Minute
	DefaultMaxOutputTokens = 16000
	DefaultEffort          = "high"
)

// efforts are the effort levels Anthropic's API takes.
var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// KeyEnv names the variable that supplies a provider's API key in scripts
// and CI: TUG_SECRET_OPENAI_API_KEY.
func KeyEnv(provider string) string {
	return "TUG_SECRET_" + strings.ToUpper(provider) + "_API_KEY"
}

// KeyRequired reports whether a provider needs an API key. A custom endpoint
// may not, as Ollama on this machine does not.
func KeyRequired(provider string) bool { return provider != Custom }

// CheckProvider returns an error for a name that is not a provider.
func CheckProvider(name string) error {
	if !slices.Contains(Providers, name) {
		return fmt.Errorf("%q is not a provider; use one of %s", name, strings.Join(Providers, ", "))
	}
	return nil
}

// ErrDisabled is returned when config.yaml does not turn explanations on.
var ErrDisabled = errors.New("--deep is off; turn it on in config.yaml with `intelligence: {enabled: true}`")

// Settings are config.yaml's intelligence settings, checked, with one call's
// overrides applied and the defaults filled in.
type Settings struct {
	Enabled            bool
	Provider           string
	Model              string
	BaseURL            string
	Effort             string
	Timeout            time.Duration
	MaxOutputTokens    int
	MonthlySpendCapUSD float64
	Prices             Prices
}

// Overrides change the provider or model for one call, as why's --provider
// and --model do.
type Overrides struct {
	Provider string
	Model    string
}

// SettingsFrom checks config.yaml's intelligence settings, applies the
// overrides and fills in the defaults. A provider other than the configured
// one gets its own address and default model, since the configured ones are
// for the configured provider.
func SettingsFrom(c config.IntelligenceConfig, o Overrides) (Settings, error) {
	s := Settings{
		Enabled:            c.Enabled,
		Provider:           strings.ToLower(strings.TrimSpace(c.Provider)),
		Model:              strings.TrimSpace(c.Model),
		BaseURL:            strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"),
		Effort:             strings.ToLower(c.Effort),
		Timeout:            c.Timeout.Duration,
		MaxOutputTokens:    c.MaxOutputTokens,
		MonthlySpendCapUSD: c.MonthlySpendCapUSD,
	}
	if s.Provider == "" {
		s.Provider = DefaultProvider
	}
	if err := CheckProvider(s.Provider); err != nil {
		return Settings{}, fmt.Errorf("intelligence.provider: %w", err)
	}
	if p := strings.ToLower(strings.TrimSpace(o.Provider)); p != "" && p != s.Provider {
		if err := CheckProvider(p); err != nil {
			return Settings{}, fmt.Errorf("--provider: %w", err)
		}
		s.Provider, s.Model, s.BaseURL = p, "", ""
	}
	if m := strings.TrimSpace(o.Model); m != "" {
		s.Model = m
	}
	s.Model = cmpOr(s.Model, DefaultModel(s.Provider))
	s.BaseURL = cmpOr(s.BaseURL, DefaultBaseURL(s.Provider))
	s.Effort = cmpOr(s.Effort, DefaultEffort)
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	if s.MaxOutputTokens <= 0 {
		s.MaxOutputTokens = DefaultMaxOutputTokens
	}
	switch {
	case s.Model == "":
		return Settings{}, fmt.Errorf("the %s provider has no default model; set intelligence.model or pass --model", s.Provider)
	case s.BaseURL == "":
		return Settings{}, fmt.Errorf("the %s provider needs intelligence.baseURL, the address of an OpenAI-compatible API such as http://localhost:11434/v1", s.Provider)
	case !slices.Contains(efforts, s.Effort):
		return Settings{}, fmt.Errorf("intelligence.effort %q is not one of %s", c.Effort, strings.Join(efforts, ", "))
	case s.MonthlySpendCapUSD < 0:
		return Settings{}, errors.New("intelligence.monthlySpendCapUSD cannot be negative")
	}
	if err := checkBaseURL(s.BaseURL); err != nil {
		return Settings{}, err
	}
	prices, err := LoadPrices(c.Prices)
	if err != nil {
		return Settings{}, err
	}
	s.Prices = prices
	return s, nil
}

// checkBaseURL refuses an address an API key would travel to in clear text,
// unless it stays on this machine.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("intelligence.baseURL %q is not a URL such as https://api.openai.com/v1", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host := u.Hostname(); host == "localhost" || net.ParseIP(host).IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("intelligence.baseURL %q must use https, so the API key is not sent in clear text", raw)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
