package intelligence

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

//go:embed prices.yaml
var pricesYAML []byte

// Price is what a model costs in US dollars per million tokens.
type Price struct {
	// Model is a model id or the start of one.
	Model    string  `json:"model"`
	Provider string  `json:"provider,omitempty"`
	Input    float64 `json:"input"`
	Output   float64 `json:"output"`
	// CacheRead is the price of a prompt token read from the provider's
	// cache; zero means a tenth of Input.
	CacheRead float64 `json:"cacheRead,omitempty"`
	Note      string  `json:"note,omitempty"`
	// Configured is true for a price from config.yaml.
	Configured bool `json:"configured,omitempty"`
}

// PriceSource says where a provider's prices came from, and when.
type PriceSource struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
	Checked  string `json:"checked"`
}

type priceTable struct {
	Sources []PriceSource `json:"sources"`
	Models  []Price       `json:"models"`
}

// builtinPrices is prices.yaml, read once.
var builtinPrices = func() priceTable {
	var t priceTable
	if err := yaml.UnmarshalStrict(pricesYAML, &t); err != nil {
		panic("intelligence: prices.yaml: " + err.Error()) // a programmer error, caught by any test
	}
	return t
}()

// Prices are config.yaml's prices, then the built-in table.
type Prices struct {
	configured []Price
}

// LoadPrices checks config.yaml's prices and puts them ahead of the
// built-in table.
func LoadPrices(configured []config.ModelPrice) (Prices, error) {
	var p Prices
	for _, c := range configured {
		model := strings.ToLower(strings.TrimSpace(c.Model))
		switch {
		case model == "":
			return Prices{}, fmt.Errorf("intelligence.prices: a price has no model")
		case c.Input < 0 || c.Output < 0 || c.CacheRead < 0:
			return Prices{}, fmt.Errorf("intelligence.prices: %s has a negative price", model)
		}
		p.configured = append(p.configured, Price{Model: model, Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, Configured: true})
	}
	return p, nil
}

// For returns the price of a model: the configured price whose model starts
// the id for the longest stretch, or else the first such row of the
// built-in table, which lists every row before any that starts it. It
// reports false for a model it has no price for.
func (p Prices) For(model string) (Price, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return Price{}, false
	}
	var best *Price
	for i, c := range p.configured {
		if strings.HasPrefix(id, c.Model) && (best == nil || len(c.Model) > len(best.Model)) {
			best = &p.configured[i]
		}
	}
	if best != nil {
		return *best, true
	}
	for _, row := range builtinPrices.Models {
		if strings.HasPrefix(id, row.Model) {
			return row, true
		}
	}
	return Price{}, false
}

// All returns every price, configured ones first.
func (p Prices) All() []Price { return slices.Concat(p.configured, builtinPrices.Models) }

// PriceSources says where the built-in prices came from.
func PriceSources() []PriceSource { return slices.Clone(builtinPrices.Sources) }

// Usage counts the tokens of one call. InputTokens excludes the tokens read
// from or written to the provider's cache, which are priced apart.
type Usage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

// The cache's prices, as Anthropic charges them, relative to the input
// price: a token read costs a tenth unless the table lists a price, and a
// token written costs a quarter more.
const (
	cacheReadShare  = 0.1
	cacheWriteShare = 1.25
)

const perMillion = 1_000_000.0

// Cost returns what a call with this usage costs.
func (pr Price) Cost(u Usage) float64 {
	read := pr.CacheRead
	if read == 0 {
		read = pr.Input * cacheReadShare
	}
	return (float64(u.InputTokens)*pr.Input +
		float64(u.OutputTokens)*pr.Output +
		float64(u.CacheReadTokens)*read +
		float64(u.CacheWriteTokens)*pr.Input*cacheWriteShare) / perMillion
}

// minOutputTokens is the least an answer is expected to take, for the low
// end of an estimate.
const minOutputTokens = 500

// Estimate returns the range a call is expected to cost: its prompt with
// the least answer, up to its prompt with the longest answer allowed.
func (pr Price) Estimate(inputTokens, maxOutputTokens int) (low, high float64) {
	low = pr.Cost(Usage{InputTokens: inputTokens, OutputTokens: min(minOutputTokens, maxOutputTokens)})
	high = pr.Cost(Usage{InputTokens: inputTokens, OutputTokens: maxOutputTokens})
	return low, high
}
