package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatFormat struct {
	Type       string      `json:"type"`
	JSONSchema *chatSchema `json:"json_schema,omitempty"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// OpenAI's current models take max_completion_tokens; Mistral and
	// most OpenAI-compatible servers take max_tokens.
	MaxCompletionTokens int        `json:"max_completion_tokens,omitempty"`
	MaxTokens           int        `json:"max_tokens,omitempty"`
	ResponseFormat      chatFormat `json:"response_format"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// chatBody builds a chat-completions request: one system message, the
// stable half first so a provider's automatic prompt cache can keep it,
// then the volatile half, then the ask. OpenAI and Mistral are held to the
// answer's schema; a custom endpoint is asked for a JSON object, which more
// servers support, and the guidance describes the form.
func (c *Client) chatBody(p Prompt) chatRequest {
	r := chatRequest{
		Model:    c.s.Model,
		Messages: []chatMessage{{Role: "system", Content: p.System()}, {Role: "user", Content: p.Ask}},
	}
	if c.s.Provider == OpenAI {
		r.MaxCompletionTokens = c.s.MaxOutputTokens
	} else {
		r.MaxTokens = c.s.MaxOutputTokens
	}
	if c.s.Provider == Custom {
		r.ResponseFormat = chatFormat{Type: "json_object"}
	} else {
		r.ResponseFormat = chatFormat{Type: "json_schema", JSONSchema: &chatSchema{Name: "tug_explanation", Strict: true, Schema: answerSchema}}
	}
	return r
}

// chatCompletions asks a model through the OpenAI chat-completions
// protocol, which OpenAI, Mistral and custom endpoints speak.
func (c *Client) chatCompletions(ctx context.Context, p Prompt) (*reply, error) {
	headers := map[string]string{}
	if c.key != "" {
		headers["authorization"] = "Bearer " + c.key
	}
	b, hdr, err := c.post(ctx, headers, c.chatBody(p))
	if err != nil {
		return nil, err
	}
	var resp chatResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("reading %s's answer: %w", c.s.Provider, err)
	}
	cached := resp.Usage.PromptTokensDetails.CachedTokens
	r := &reply{
		model:     resp.Model,
		requestID: cmpOr(hdr.Get("x-request-id"), resp.ID),
		usage: Usage{
			// The prompt's count includes what was read from the cache,
			// which is priced apart.
			InputTokens:     max(resp.Usage.PromptTokens-cached, 0),
			OutputTokens:    resp.Usage.CompletionTokens,
			CacheReadTokens: cached,
		},
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("%s's answer has no choices", c.s.Provider)
	}
	choice := resp.Choices[0]
	r.text = strings.TrimSpace(choice.Message.Content)
	switch {
	case choice.Message.Refusal != "" || choice.FinishReason == "content_filter":
		r.stop = stopRefused
	case choice.FinishReason == "length":
		r.stop = stopTruncated
	}
	return r, nil
}
