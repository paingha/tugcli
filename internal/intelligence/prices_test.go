package intelligence

import (
	"math"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/config"
)

// TestPriceTableOrder holds prices.yaml to its rule: a row comes before any
// row that starts it, so reading the table top down finds the longest
// prefix, and no model is listed twice.
func TestPriceTableOrder(t *testing.T) {
	rows := builtinPrices.Models
	seen := map[string]bool{}
	for i, row := range rows {
		if seen[row.Model] {
			t.Errorf("%s is listed twice", row.Model)
		}
		seen[row.Model] = true
		if row.Model != strings.ToLower(row.Model) || row.Input <= 0 || row.Output <= 0 || CheckProvider(row.Provider) != nil {
			t.Errorf("row %+v is not a lowercase model with a provider and prices", row)
		}
		for _, later := range rows[i+1:] {
			if strings.HasPrefix(later.Model, row.Model) {
				t.Errorf("%s comes before %s, which it starts, so %s would take its price", row.Model, later.Model, later.Model)
			}
		}
	}
	for _, provider := range []string{Anthropic, OpenAI, Mistral} {
		found := false
		for _, s := range PriceSources() {
			if s.Provider == provider && strings.HasPrefix(s.URL, "https://") && s.Checked != "" {
				found = true
			}
		}
		if !found {
			t.Errorf("prices.yaml names no source and date for %s", provider)
		}
	}
}

func TestPricesFor(t *testing.T) {
	configured, err := LoadPrices([]config.ModelPrice{
		{Model: "claude-opus-5-5", Input: 3, Output: 15},
		{Model: "llama", Input: 0.05, Output: 0.1},
		{Model: "llama4", Input: 0.2, Output: 0.4},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		model  string
		prices Prices
		want   float64 // input price, or -1 for unknown
	}{
		{model: "claude-opus-5-5", want: 4},
		{model: "claude-opus-5-5-20261101", want: 4}, // a dated id takes its family's price
		{model: "claude-opus-5", want: 5},
		{model: "claude-opus-5-20260601", want: 5},
		{model: "claude-sonnet-5-5", want: 2},
		{model: "claude-fable-5-2", want: 8},
		{model: "claude-fable-5", want: 10},
		{model: "Claude-Haiku-5-5", want: 0.1},
		{model: "gpt-6.2-astra", want: 8},
		{model: "gpt-6-luna", want: 0.1},
		{model: "mistral-large-latest", want: 0.7},
		{model: "claude-opus-4-5", want: -1}, // not in the table: unknown, never zero
		{model: "gpt-4o", want: -1},
		{model: "", want: -1},
		{model: "claude-opus-5-5", prices: configured, want: 3}, // config.yaml overrides
		{model: "claude-sonnet-5-5", prices: configured, want: 2},
		{model: "llama4:70b", prices: configured, want: 0.2}, // the longest configured prefix
		{model: "llama3.3", prices: configured, want: 0.05},
	}
	for _, tt := range tests {
		price, ok := tt.prices.For(tt.model)
		got := price.Input
		if !ok {
			got = -1
		}
		if got != tt.want {
			t.Errorf("For(%q) = %v (%v), want %v", tt.model, got, ok, tt.want)
		}
	}
}

func TestCost(t *testing.T) {
	opus, _ := Prices{}.For("claude-opus-5-5")
	sonnet4, _ := Prices{}.For("claude-sonnet-4-6")
	tests := []struct {
		name  string
		price Price
		usage Usage
		want  float64
	}{
		{name: "input and output", price: opus, usage: Usage{InputTokens: 1_000_000, OutputTokens: 100_000}, want: 4 + 2},
		{name: "a listed cache-read price", price: opus, usage: Usage{CacheReadTokens: 1_000_000}, want: 0.2},
		{name: "a tenth of input without one", price: sonnet4, usage: Usage{CacheReadTokens: 1_000_000}, want: 0.3},
		{name: "cache writes at a quarter more", price: sonnet4, usage: Usage{CacheWriteTokens: 1_000_000}, want: 3.75},
		{name: "nothing", price: opus, want: 0},
	}
	for _, tt := range tests {
		if got := tt.price.Cost(tt.usage); math.Abs(got-tt.want) > 1e-12 {
			t.Errorf("%s: Cost(%+v) = %v, want %v", tt.name, tt.usage, got, tt.want)
		}
	}
	low, high := opus.Estimate(10_000, 16_000)
	if math.Abs(low-(10_000*4+500*20)/1e6) > 1e-12 || math.Abs(high-(10_000*4+16_000*20)/1e6) > 1e-12 {
		t.Errorf("Estimate(10000, 16000) = %v, %v", low, high)
	}
	if low, high := opus.Estimate(1000, 100); low != high {
		t.Errorf("Estimate with a limit under the least answer = %v, %v, want equal", low, high)
	}
}

func TestLoadPricesRefusesNegativePrices(t *testing.T) {
	if _, err := LoadPrices([]config.ModelPrice{{Model: "x", Input: -1}}); err == nil || err.Error() != "intelligence.prices: x has a negative price" {
		t.Errorf("LoadPrices error = %v", err)
	}
}
