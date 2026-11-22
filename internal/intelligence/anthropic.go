package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// anthropicVersion is the Messages API version tug speaks.
const anthropicVersion = "2026-10-21"

// fallbackBeta and fallbackModels: these models take server-side fallbacks,
// which retry a request they decline on the model Anthropic recommends for
// the reason they declined.
const fallbackBeta = "server-side-fallback-2026-07-01"

var fallbackModels = []string{"claude-fable-5-2", "claude-fable-5-1", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5"}

// noEffort are the starts of model ids that reject the effort setting.
var noEffort = []string{"claude-3", "claude-haiku-4", "claude-sonnet-4-5", "claude-sonnet-4-2", "claude-opus-4-1", "claude-opus-4-2"}

type anthropicText struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl *anthropicType `json:"cache_control,omitempty"`
}

type anthropicType struct {
	Type string `json:"type"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type jsonSchemaFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anthropicOutput struct {
	Effort string           `json:"effort,omitempty"`
	Format jsonSchemaFormat `json:"format"`
}

type anthropicRequest struct {
	Model        string             `json:"model"`
	MaxTokens    int                `json:"max_tokens"`
	System       []anthropicText    `json:"system"`
	Messages     []anthropicMessage `json:"messages"`
	OutputConfig anthropicOutput    `json:"output_config"`
	Fallbacks    string             `json:"fallbacks,omitempty"`
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason  string `json:"stop_reason"`
	StopDetails *struct {
		Category string `json:"category"`
	} `json:"stop_details"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// anthropicBody builds a Messages API request: the stable half as the
// first system block, with a cache breakpoint, then the volatile half, then
// the ask; the answer constrained to the schema.
func (c *Client) anthropicBody(p Prompt) anthropicRequest {
	r := anthropicRequest{
		Model:     c.s.Model,
		MaxTokens: c.s.MaxOutputTokens,
		System: []anthropicText{
			{Type: "text", Text: p.Stable, CacheControl: &anthropicType{Type: "ephemeral"}},
			{Type: "text", Text: p.Volatile},
		},
		Messages:     []anthropicMessage{{Role: "user", Content: p.Ask}},
		OutputConfig: anthropicOutput{Format: jsonSchemaFormat{Type: "json_schema", Schema: answerSchema}},
	}
	if !slices.ContainsFunc(noEffort, func(prefix string) bool { return strings.HasPrefix(r.Model, prefix) }) {
		r.OutputConfig.Effort = c.s.Effort
	}
	if slices.Contains(fallbackModels, r.Model) {
		r.Fallbacks = "default"
	}
	return r
}

// anthropic asks Claude through the Messages API.
func (c *Client) anthropic(ctx context.Context, p Prompt) (*reply, error) {
	body := c.anthropicBody(p)
	headers := map[string]string{"x-api-key": c.key, "anthropic-version": anthropicVersion}
	if body.Fallbacks != "" {
		headers["anthropic-beta"] = fallbackBeta
	}
	b, hdr, err := c.post(ctx, headers, body)
	if err != nil {
		return nil, err
	}
	var resp anthropicResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("reading %s's answer: %w", c.s.Provider, err)
	}
	r := &reply{
		model:     resp.Model,
		requestID: cmpOr(hdr.Get("request-id"), resp.ID),
		usage: Usage{
			InputTokens:      resp.Usage.InputTokens,
			OutputTokens:     resp.Usage.OutputTokens,
			CacheReadTokens:  resp.Usage.CacheReadInputTokens,
			CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
		},
	}
	// A fallback block marks where a model that declined handed over to
	// another; the answer is the text after the last one.
	start := 0
	for i, block := range resp.Content {
		if block.Type == "fallback" {
			start = i + 1
		}
	}
	var text strings.Builder
	for _, block := range resp.Content[start:] {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	r.text = text.String()
	switch resp.StopReason {
	case "refusal":
		r.stop = stopRefused
		if resp.StopDetails != nil {
			r.category = resp.StopDetails.Category
		}
	case "max_tokens":
		r.stop = stopTruncated
	}
	return r, nil
}
