package intelligence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The limits on one call.
const (
	// maxResponse bounds what is read of a response.
	maxResponse = 4 << 20
	// retries is how many times a request is retried when the provider is
	// busy or down.
	retries = 2
	// maxRetryAfter caps how long a retry-after header can make tug wait.
	maxRetryAfter = 30 * time.Second
)

// ErrRefused is returned when the model declines to answer.
var ErrRefused = errors.New("the model declined to explain this evidence")

// ErrTruncated is returned when the answer was cut off before it was
// complete.
var ErrTruncated = errors.New("the model's answer was cut off before it was complete; raise intelligence.maxOutputTokens")

// APIError is an error response from a provider. Its message never holds
// the API key.
type APIError struct {
	Provider string
	Status   int
	// Type is the provider's error type, such as authentication_error.
	Type      string
	Message   string
	RequestID string
	// final is set when the provider says not to retry.
	final bool
}

// Error says what the provider answered, with its request ID for the
// provider's support.
func (e *APIError) Error() string {
	s := fmt.Sprintf("%s answered %d", e.Provider, e.Status)
	if e.Type != "" {
		s += " " + e.Type
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// Unauthorized reports whether the provider rejected the key.
func (e *APIError) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// retryable reports whether the request may succeed when tried again.
func (e *APIError) retryable() bool {
	return !e.final && (e.Status == http.StatusTooManyRequests || e.Status >= 500)
}

// Client asks the configured provider for explanations.
type Client struct {
	s         Settings
	key       string
	http      *http.Client
	userAgent string
	now       func() time.Time
	// backoff is how long to wait before retry n, from 1, when the
	// provider does not say.
	backoff func(n int) time.Duration
}

// Option sets up a Client.
type Option func(*Client)

// WithHTTPClient sends requests through h instead of a client with the
// configured timeout.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithUserAgent names the program in requests.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// WithClock measures latency and stamps calls with now instead of
// time.Now.
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// NewClient returns a client for settings from SettingsFrom that
// authenticates with key, which may be empty for a custom endpoint.
func NewClient(s Settings, key string, opts ...Option) *Client {
	c := &Client{
		s:         s,
		key:       key,
		http:      &http.Client{Timeout: s.Timeout},
		userAgent: "tug",
		now:       time.Now,
		backoff:   func(n int) time.Duration { return time.Duration(n) * time.Second },
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Endpoint is where requests go.
func (c *Client) Endpoint() string {
	if c.s.Provider == Anthropic {
		return c.s.BaseURL + "/v1/messages"
	}
	return c.s.BaseURL + "/chat/completions"
}

// reply is what a provider answered, in common terms.
type reply struct {
	text      string
	model     string
	usage     Usage
	requestID string
	// stop is "", stopRefused or stopTruncated.
	stop     string
	category string
}

const (
	stopRefused   = "refused"
	stopTruncated = "truncated"
)

// complete sends a prompt to the configured provider.
func (c *Client) complete(ctx context.Context, p Prompt) (*reply, error) {
	if c.s.Provider == Anthropic {
		return c.anthropic(ctx, p)
	}
	return c.chatCompletions(ctx, p)
}

// post sends a JSON request, retrying while the provider is busy or down,
// and returns the response's body and headers.
func (c *Client) post(ctx context.Context, headers map[string]string, body any) ([]byte, http.Header, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; ; attempt++ {
		resp, hdr, wait, err := c.postOnce(ctx, headers, b)
		if err == nil {
			return resp, hdr, nil
		}
		var apiErr *APIError
		retry := ctx.Err() == nil // a network error, or a busy provider
		if errors.As(err, &apiErr) {
			retry = apiErr.retryable()
		}
		if !retry || attempt == retries {
			return nil, nil, err
		}
		if wait < 0 {
			wait = c.backoff(attempt + 1)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// postOnce sends one request. On an error it also returns how long the
// provider asked to wait before trying again, or -1 when it did not say.
func (c *Client) postOnce(ctx context.Context, headers map[string]string, body []byte) ([]byte, http.Header, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, -1, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", c.userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, -1, ctx.Err()
		}
		return nil, nil, -1, fmt.Errorf("asking %s: %s", c.s.Provider, c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, nil, -1, fmt.Errorf("reading %s's answer: %w", c.s.Provider, err)
	}
	if resp.StatusCode/100 == 2 {
		return b, resp.Header, -1, nil
	}
	apiErr := &APIError{
		Provider:  c.s.Provider,
		Status:    resp.StatusCode,
		RequestID: cmpOr(resp.Header.Get("request-id"), resp.Header.Get("x-request-id")),
		final:     resp.Header.Get("x-should-retry") == "false",
	}
	apiErr.Type, apiErr.Message = errorBody(b)
	apiErr.Message = c.scrub(apiErr.Message)
	wait := time.Duration(-1)
	if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil && s >= 0 {
		wait = min(time.Duration(s)*time.Second, maxRetryAfter)
	}
	return nil, nil, wait, apiErr
}

// errorBody reads a provider's error type and message, in Anthropic's and
// OpenAI's {"error": {...}} form, Mistral's {"message": ...} form or a
// {"detail": ...} one.
func errorBody(b []byte) (typ, msg string) {
	var e struct {
		Error *struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Detail  any    `json:"detail"`
	}
	if json.Unmarshal(b, &e) != nil {
		return "", ""
	}
	if e.Error != nil {
		typ = e.Error.Type
		if s, ok := e.Error.Code.(string); ok && typ == "" {
			typ = s
		}
		return typ, e.Error.Message
	}
	if d, ok := e.Detail.(string); ok && e.Message == "" {
		return e.Type, d
	}
	if e.Type == "error" {
		e.Type = ""
	}
	return e.Type, e.Message
}

// keyShapes are how providers show a key, whole or masked, in their error
// messages: "Incorrect API key provided: sk-proj-****abcd".
var keyShapes = regexp.MustCompile(`\b(?:sk|pk|key)-[A-Za-z0-9_*.-]{4,}`)

// scrub takes the API key, and anything shaped like one, out of text that
// may reach an error message.
func (c *Client) scrub(s string) string {
	if c.key != "" {
		s = strings.ReplaceAll(s, c.key, "[key]")
	}
	return keyShapes.ReplaceAllString(s, "[key]")
}
