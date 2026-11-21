// Package ssm reaches private endpoints through AWS Systems Manager Session
// Manager port forwarding, so a cluster in a private VPC is reachable with
// only tug installed: no AWS CLI and no session-manager-plugin.
//
// Dial starts a session with the AWS-StartPortForwardingSessionToRemoteHost
// document on a managed instance, opens the session's data channel over
// WebSocket and speaks the Session Manager data channel protocol on it: the
// binary agent messages, the handshake, acknowledgements and
// retransmission. The session carries exactly one TCP connection from the
// instance to the requested address, and the returned net.Conn is that
// connection.
//
// tug asks the agent for basic port forwarding rather than the multiplexed
// mode the current plugin uses (see clientVersion). In that mode the agent
// does not report the far end closing its TCP connection: it dials again
// when more data arrives. In-band ends such as a TLS close_notify or an
// HTTP/2 GOAWAY still reach the reader, and the session ends when the
// connection is closed here, when Session Manager closes the channel, or
// when the agent cannot connect.
package ssm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/gorilla/websocket"
)

// Scheme is the URL scheme of SSM tunnels.
const Scheme = "ssm"

// DocumentName is the AWS-managed Session document Dial starts. It forwards
// one TCP connection from the instance to a host and port it can reach.
const DocumentName = "AWS-StartPortForwardingSessionToRemoteHost"

// Config is one SSM tunnel: the managed instance that forwards traffic.
type Config struct {
	Target  string // instance ID, e.g. i-0123456789abcdef0
	Region  string // optional; else from the profile
	Profile string // optional AWS profile
}

// ErrKMSEncryption is returned by Dial when the account's Session Manager
// preferences require KMS encryption of session data, which tug does not
// support.
var ErrKMSEncryption = errors.New("the Session Manager preferences require KMS encryption, which tug does not support; " +
	"turn off KMS encryption in the Session Manager preferences to tunnel through SSM")

// ErrSessionClosed is returned by Read and Write once Session Manager has
// closed the session, wrapped with the reason it gave if there was one.
var ErrSessionClosed = errors.New("the SSM session was closed")

var (
	// targetPattern matches EC2 instance IDs (i-) and the IDs of other
	// managed nodes (mi-), which have 8 or 17 hex digits.
	targetPattern = regexp.MustCompile(`^m?i-[0-9a-f]{8}([0-9a-f]{9})?$`)
	// regionPattern keeps region names to the characters of a DNS label.
	regionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)+$`)
)

// ParseURL reads ssm://<target>?region=..&profile=..
func ParseURL(u *url.URL) (Config, error) {
	if u == nil {
		return Config{}, errors.New("no SSM tunnel URL")
	}
	if u.Scheme != Scheme {
		return Config{}, fmt.Errorf("tunnel URL %q is not an ssm:// URL", u.Redacted())
	}
	if u.Opaque != "" || u.Host == "" {
		return Config{}, fmt.Errorf("tunnel URL %q names no instance; write ssm://<instance-id>", u.Redacted())
	}
	if u.User != nil || u.Port() != "" || (u.Path != "" && u.Path != "/") || u.Fragment != "" {
		return Config{}, fmt.Errorf("tunnel URL %q has more than an instance ID and a query; write ssm://<instance-id>?region=<region>&profile=<profile>", u.Redacted())
	}
	cfg := Config{Target: strings.ToLower(u.Hostname())}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Config{}, fmt.Errorf("reading the query of tunnel URL %q: %w", u.Redacted(), err)
	}
	for key, values := range query {
		if len(values) != 1 {
			return Config{}, fmt.Errorf("tunnel URL %q sets %s more than once", u.Redacted(), key)
		}
		switch key {
		case "region":
			cfg.Region = values[0]
		case "profile":
			cfg.Profile = values[0]
		default:
			return Config{}, fmt.Errorf("tunnel URL %q has unknown parameter %q; the parameters are region and profile", u.Redacted(), key)
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("tunnel URL %q: %w", u.Redacted(), err)
	}
	return cfg, nil
}

// validate checks the target and region.
func (c Config) validate() error {
	if !targetPattern.MatchString(c.Target) {
		return fmt.Errorf("target %q is not a managed instance ID such as i-0123456789abcdef0", c.Target)
	}
	if c.Region != "" && !regionPattern.MatchString(c.Region) {
		return fmt.Errorf("region %q is not an AWS region such as us-east-1", c.Region)
	}
	return nil
}

// API is the part of the SSM client that Dial uses.
type API interface {
	StartSession(ctx context.Context, in *ssm.StartSessionInput, opts ...func(*ssm.Options)) (*ssm.StartSessionOutput, error)
	TerminateSession(ctx context.Context, in *ssm.TerminateSessionInput, opts ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error)
}

var _ API = (*ssm.Client)(nil)

// Option configures Dial.
type Option func(*options)

type options struct {
	api      API
	endpoint string
	dialer   *websocket.Dialer
	log      *slog.Logger
}

// WithAPI replaces the SSM client that Dial builds from the AWS profile, for
// tests. The data channel is then opened without request signing, which
// relies on the session token alone, as older Session Manager plugins do.
func WithAPI(api API) Option { return func(o *options) { o.api = api } }

// WithEndpoint sends SSM API requests to endpoint, such as an interface VPC
// endpoint, instead of the regional one.
func WithEndpoint(endpoint string) Option { return func(o *options) { o.endpoint = endpoint } }

// WithDialer opens the data channel WebSocket with d instead of
// websocket.DefaultDialer, for proxies and tests.
func WithDialer(d *websocket.Dialer) Option { return func(o *options) { o.dialer = d } }

// WithLogger logs the session's progress to l. Dial logs nothing by default.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// Dial opens a port-forwarding session through Target to address (host:port,
// e.g. the EKS private endpoint) and returns it as a net.Conn.
//
// The context bounds starting the session and the handshake; once Dial has
// returned, cancelling it does not affect the connection. Closing the
// connection ends the session. Dial loads the AWS configuration on every
// call; a Dialer loads it once for many connections.
func Dial(ctx context.Context, cfg Config, address string, opts ...Option) (net.Conn, error) {
	d, err := NewDialer(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return d.Dial(ctx, address)
}

// splitAddress checks that address is host:port and returns its parts.
func splitAddress(address string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("reading tunnel address %q: %w", address, err)
	}
	if host == "" {
		return "", "", fmt.Errorf("tunnel address %q has no host", address)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("tunnel address %q has no port between 1 and 65535", address)
	}
	return host, port, nil
}

// startError explains why StartSession failed, with what to do about the
// common causes.
func startError(target string, err error) error {
	var (
		notConnected *ssmtypes.TargetNotConnected
		badTarget    *ssmtypes.InvalidTarget
	)
	switch {
	case errors.As(err, &notConnected):
		return fmt.Errorf("starting an SSM session on %s: the instance is not connected to Session Manager; "+
			"check that it is running, its SSM agent is online and it can reach the ssm and ssmmessages endpoints: %w", target, err)
	case errors.As(err, &badTarget):
		return fmt.Errorf("starting an SSM session on %s: no such managed instance in this account and region; "+
			"check the instance ID and the region: %w", target, err)
	}
	return fmt.Errorf("starting an SSM session on %s: %w", target, err)
}
