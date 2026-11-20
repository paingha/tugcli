package iap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/oauth2"
	googleauth "golang.org/x/oauth2/google"

	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// DefaultRelayURL is the IAP TCP forwarding relay. The connect and
// reconnect endpoints are under it.
const DefaultRelayURL = "wss://tunnel.cloudproxy.app/v4"

// DefaultReconnectTimeout is how long a tunnel keeps trying to resume its
// session after the relay drops it, as gcloud does.
const DefaultReconnectTimeout = 15 * time.Minute

// Settings from gcloud's iap_tunnel_websocket*.py.
const (
	// origin is the Origin header gcloud's tunnel sends.
	origin = "bot:iap-tunneler"
	// scope is the OAuth scope of the access token.
	scope = "https://www.googleapis.com/auth/cloud-platform"
	// openTimeout bounds the WebSocket handshake and the wait for the
	// relay's first frame.
	openTimeout = 60 * time.Second
	// reconnectWait and maxReconnectWait shape the back-off between
	// attempts to resume; each wait is 1.1 times the one before.
	reconnectWait    = 1500 * time.Millisecond
	maxReconnectWait = 20 * time.Second
)

// Close codes the relay sends, from Google's IAP FAQ and gcloud.
const (
	closeBackendUnreachable = 4003 // the relay could not connect to the port
	closeReauthRequired     = 4004 // the session lives on, with new credentials
	closeDestinationClosed  = 4010 // the destination closed the connection
	closeNotAuthorized      = 4033 // no permission, or the instance is not there or not running
	closeInstanceNotFound   = 4047 // the instance does not exist or is stopped
)

// Hints for errors people will read.
const (
	adcHint        = "gcloud auth application-default login"
	tunnelUserHint = "you need the IAP-secured Tunnel User role (roles/iap.tunnelResourceAccessor) on the instance or destination group; an instance must also exist and be running"
)

// maxMessage bounds one WebSocket message from the relay. gcloud sends
// DATA frames of at most 16 KiB; this leaves room in case the relay's are
// larger.
const maxMessage = 1 << 20

// Option configures Dial.
type Option func(*options)

type options struct {
	tokens           oauth2.TokenSource
	relayURL         string
	tlsConfig        *tls.Config
	userAgent        string
	logger           *slog.Logger
	reconnectTimeout time.Duration
	reconnectWait    time.Duration
	maxReconnectWait time.Duration
}

// WithTokenSource sets where access tokens come from. The default is
// DefaultTokenSource, made for each Dial; a caller that opens many tunnels
// can make one and share it.
func WithTokenSource(ts oauth2.TokenSource) Option {
	return func(o *options) { o.tokens = ts }
}

// WithRelayURL replaces DefaultRelayURL, as gcloud's
// --iap-tunnel-url-override does. It must use wss unless it is on this
// machine, as a test relay is. A URL without a path gets /v4.
func WithRelayURL(u string) Option {
	return func(o *options) { o.relayURL = u }
}

// WithTLSConfig sets the TLS settings for the relay connection.
func WithTLSConfig(c *tls.Config) Option {
	return func(o *options) { o.tlsConfig = c }
}

// WithUserAgent sets the User-Agent header. The default is tug/<version>.
func WithUserAgent(ua string) Option {
	return func(o *options) { o.userAgent = ua }
}

// WithLogger logs resumed sessions and frames the tunnel skips, at Debug.
// The default discards them.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithReconnectTimeout sets how long a tunnel keeps trying to resume its
// session after the relay drops it. Zero or less turns resuming off, so the
// first drop ends the tunnel. The default is DefaultReconnectTimeout.
func WithReconnectTimeout(d time.Duration) Option {
	return func(o *options) { o.reconnectTimeout = d }
}

// withReconnectBackoff shortens the waits between attempts to resume, for
// tests.
func withReconnectBackoff(first, most time.Duration) Option {
	return func(o *options) { o.reconnectWait, o.maxReconnectWait = first, most }
}

// DefaultTokenSource returns access tokens from Application Default
// Credentials with the cloud-platform scope. ctx is used for refreshing
// tokens later too, so it should outlive the tunnels that use them.
func DefaultTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	ts, err := googleauth.DefaultTokenSource(ctx, scope)
	if err != nil {
		return nil, noCredentials(err)
	}
	return ts, nil
}

func noCredentials(err error) error {
	return provider.Errorf(provider.Unauthenticated, "no Google credentials: %v", err).WithHint(adcHint)
}

// client opens WebSockets to the relay for one tunnel.
type client struct {
	relay            *url.URL
	tokens           oauth2.TokenSource
	dialer           *websocket.Dialer
	userAgent        string
	logger           *slog.Logger
	reconnectTimeout time.Duration
	reconnectWait    time.Duration
	maxReconnectWait time.Duration
}

func newClient(ctx context.Context, opts []Option) (*client, error) {
	o := options{
		relayURL:         DefaultRelayURL,
		userAgent:        "tug/" + version.String(),
		reconnectTimeout: DefaultReconnectTimeout,
		reconnectWait:    reconnectWait,
		maxReconnectWait: maxReconnectWait,
	}
	for _, opt := range opts {
		opt(&o)
	}
	relay, err := parseRelay(o.relayURL)
	if err != nil {
		return nil, err
	}
	if o.tokens == nil {
		// The token source refreshes with the context it was made with,
		// long after Dial returns, so it must not end with Dial's.
		if o.tokens, err = DefaultTokenSource(context.WithoutCancel(ctx)); err != nil {
			return nil, err
		}
	}
	if o.logger == nil {
		o.logger = slog.New(slog.DiscardHandler)
	}
	return &client{
		relay:  relay,
		tokens: o.tokens,
		dialer: &websocket.Dialer{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: o.tlsConfig,
			Subprotocols:    []string{subprotocol},
		},
		userAgent:        o.userAgent,
		logger:           o.logger,
		reconnectTimeout: o.reconnectTimeout,
		reconnectWait:    o.reconnectWait,
		maxReconnectWait: o.maxReconnectWait,
	}, nil
}

// parseRelay checks a relay URL. Like gcloud's override, a path other than
// / replaces /v4.
func parseRelay(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parsing the IAP relay URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("the IAP relay URL %s has no host", u.Redacted())
	}
	if u.Scheme != "wss" && (u.Scheme != "ws" || !isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("the IAP relay URL %s must use wss", u.Redacted())
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v4"
	}
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// endpoint returns the URL of the relay endpoint name with query q.
func (cl *client) endpoint(name string, q url.Values) string {
	u := *cl.relay
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + name
	u.RawQuery = q.Encode()
	return u.String()
}

// Dial opens an IAP TCP forwarding connection to address (host:port; in
// instance mode only the port is used) and returns it as a net.Conn.
//
// ctx bounds opening the tunnel only, as with net.Dialer: once Dial has
// returned, cancelling ctx does not affect the connection. Close it to end
// it.
func Dial(ctx context.Context, cfg Config, address string, opts ...Option) (net.Conn, error) {
	t, err := targetFor(cfg, address)
	if err != nil {
		return nil, err
	}
	cl, err := newClient(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("opening an IAP tunnel to %s: %w", t, err)
	}
	ws, f, err := cl.open(ctx, cl.endpoint("connect", t.connectQuery()), t, tagConnectSuccessSID)
	if err != nil {
		return nil, fmt.Errorf("opening an IAP tunnel to %s: %w", t, err)
	}
	return newConn(ctx, cl, t, ws, string(f.data)), nil
}

// open dials the relay endpoint and waits for its first frame, which must
// have the tag want.
func (cl *client) open(ctx context.Context, endpoint string, t target, want uint16) (*websocket.Conn, frame, error) {
	ctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	tok, err := cl.tokens.Token()
	if err != nil {
		return nil, frame{}, tokenError(err)
	}
	if tok.AccessToken == "" {
		return nil, frame{}, noCredentials(errors.New("the token source gave an empty access token"))
	}
	h := http.Header{}
	h.Set("User-Agent", cl.userAgent)
	h.Set("Origin", origin)
	h.Set("Authorization", "Bearer "+tok.AccessToken)
	ws, resp, err := cl.dialer.DialContext(ctx, endpoint, h)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		if errors.Is(err, websocket.ErrBadHandshake) && resp != nil {
			return nil, frame{}, handshakeError(resp, t)
		}
		return nil, frame{}, fmt.Errorf("reaching the IAP relay at %s: %w", cl.relay.Host, err)
	}
	if got := ws.Subprotocol(); got != subprotocol {
		_ = ws.Close()
		return nil, frame{}, fmt.Errorf("%w: it chose the WebSocket subprotocol %q, not %s", errProtocol, got, subprotocol)
	}
	ws.SetReadLimit(maxMessage)
	f, err := readFirst(ctx, ws, t, want)
	if err != nil {
		_ = ws.Close()
		return nil, frame{}, err
	}
	return ws, f, nil
}

// readFirst reads the frame that opens or resumes a session.
func readFirst(ctx context.Context, ws *websocket.Conn, t target, want uint16) (frame, error) {
	// Closing the WebSocket ends the read when ctx ends. A read deadline
	// could fire first and hide that it was ctx.
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	mt, msg, err := ws.ReadMessage()
	if !stop() {
		return frame{}, fmt.Errorf("waiting for the IAP relay to open the tunnel: %w", ctx.Err())
	}
	if err != nil {
		var ce *websocket.CloseError
		if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure {
			return frame{}, relayClosed(ce.Code, ce.Text, t)
		}
		return frame{}, fmt.Errorf("waiting for the IAP relay to open the tunnel: %w", err)
	}
	if mt != websocket.BinaryMessage {
		return frame{}, fmt.Errorf("%w: a text message instead of %s", errProtocol, tagName(want))
	}
	f, err := parseFrame(msg)
	if err != nil {
		return frame{}, err
	}
	if f.tag != want {
		return frame{}, fmt.Errorf("%w: %s instead of %s", errProtocol, tagName(f.tag), tagName(want))
	}
	return f, nil
}

// tokenError classifies a failure to get an access token. A refused
// refresh means signing in again; anything else may pass.
func tokenError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return noCredentials(err)
	}
	return provider.Errorf(provider.Unavailable, "getting a Google access token: %v", err)
}

// handshakeError reads the relay's refusal of the WebSocket upgrade.
func handshakeError(resp *http.Response, t target) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := resp.Status
	if b := strings.Join(strings.Fields(string(body)), " "); b != "" {
		detail += ": " + b
	}
	e := provider.StatusError(resp.StatusCode, fmt.Sprintf("the IAP relay refused the tunnel to %s: %s", t, detail), resp.Header)
	switch e.Code {
	case provider.Unauthenticated:
		return e.WithHint(adcHint)
	case provider.PermissionDenied:
		return e.WithHint(tunnelUserHint)
	case provider.NotFound:
		return e.WithHint(t.notFoundHint())
	}
	return e
}

// closeError is a close frame that ended a tunnel. It unwraps to a
// *provider.Error with a hint for the codes Google documents.
type closeError struct {
	code int
	err  *provider.Error
}

func (e *closeError) Error() string { return e.err.Error() }
func (e *closeError) Unwrap() error { return e.err }

// relayClosed describes the close frame the relay sent.
func relayClosed(code int, reason string, t target) *closeError {
	what := fmt.Sprintf("the IAP relay closed the tunnel to %s [%d]", t, code)
	if reason != "" {
		what = fmt.Sprintf("the IAP relay closed the tunnel to %s [%d: %s]", t, code, reason)
	}
	var e *provider.Error
	switch code {
	case closeBackendUnreachable:
		e = provider.Errorf(provider.Unavailable, "%s: it could not connect to the port", what).
			WithHint(fmt.Sprintf("check that something listens on port %d and that the firewall allows TCP to it from 35.235.240.0/20, the range IAP connects from", t.port))
	case closeReauthRequired:
		e = provider.Errorf(provider.Unauthenticated, "%s: it needs fresh credentials", what).WithHint(adcHint)
	case closeDestinationClosed:
		e = provider.Errorf(provider.Unavailable, "%s: the destination closed the connection", what)
	case closeNotAuthorized:
		e = provider.Errorf(provider.PermissionDenied, "%s: not authorised", what).WithHint(tunnelUserHint)
	case closeInstanceNotFound:
		e = provider.Errorf(provider.NotFound, "%s: the instance does not exist or is not running", what).WithHint(t.notFoundHint())
	default:
		e = provider.Errorf(provider.Unavailable, "%s", what)
	}
	return &closeError{code: code, err: e}
}
