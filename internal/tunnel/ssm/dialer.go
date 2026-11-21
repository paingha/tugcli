package ssm

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/gorilla/websocket"

	"github.com/paingha/tugcli/internal/version"
)

// Dialer opens connections through one SSM target. It loads the AWS
// configuration on its first Dial and shares the clients and the
// credential cache among every later one, so a tunnel pool does not read
// the profile for each connection. A failed load is not kept: the next Dial
// tries again, which picks up a renewed sign-in.
type Dialer struct {
	cfg Config
	o   options

	mu      sync.Mutex
	clients *clients
}

// clients are what a Dialer shares between sessions.
type clients struct {
	api API
	// kms sets up KMS-encrypted sessions; nil when the API was injected
	// without one.
	kms KMSAPI
	// signer signs the data channel request; nil when the API was injected.
	signer *streamSigner
}

// NewDialer returns a Dialer for cfg.
func NewDialer(cfg Config, opts ...Option) (*Dialer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	o := options{dialer: websocket.DefaultDialer, log: slog.New(slog.DiscardHandler), resumeDelay: resumeDelay}
	for _, opt := range opts {
		opt(&o)
	}
	return &Dialer{cfg: cfg, o: o}, nil
}

// Dial opens a port-forwarding session to address, as the package's Dial
// does, with the Dialer's shared clients.
func (d *Dialer) Dial(ctx context.Context, address string) (net.Conn, error) {
	host, port, err := splitAddress(address)
	if err != nil {
		return nil, err
	}
	cl, err := d.load(ctx)
	if err != nil {
		return nil, err
	}
	c, err := d.startSession(ctx, cl, address, host, port)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Close releases what the Dialer holds. Connections it returned stay open
// until they are closed themselves.
func (d *Dialer) Close() error { return nil }

// load returns the shared clients, loading them the first time, and checks
// that the credentials still resolve.
func (d *Dialer) load(ctx context.Context) (*clients, error) {
	d.mu.Lock()
	cl := d.clients
	if cl == nil {
		var err error
		if cl, err = d.o.newClients(ctx, d.cfg); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		d.clients = cl
	}
	d.mu.Unlock()
	if cl.signer != nil {
		// The SDK caches credentials and refreshes them before they expire,
		// so this is cheap, and an expired sign-in is reported as such.
		if _, err := cl.signer.creds.Retrieve(ctx); err != nil {
			return nil, fmt.Errorf("resolving AWS credentials%s; sign in again or refresh the profile's credentials: %w", profileSuffix(d.cfg.Profile), err)
		}
	}
	return cl, nil
}

// startSession starts one Session Manager session to address and opens its
// data channel.
func (d *Dialer) startSession(ctx context.Context, cl *clients, address, host, port string) (*conn, error) {
	out, err := cl.api.StartSession(ctx, &ssm.StartSessionInput{
		Target:       awsv2.String(d.cfg.Target),
		DocumentName: awsv2.String(DocumentName),
		// localPortNumber only tells the Session Manager plugin where to
		// listen; the document defaults it, and the agent ignores it.
		Parameters: map[string][]string{"host": {host}, "portNumber": {port}},
	})
	if err != nil {
		return nil, startError(d.cfg.Target, err)
	}
	sessionID := awsv2.ToString(out.SessionId)
	streamURL := awsv2.ToString(out.StreamUrl)
	token := awsv2.ToString(out.TokenValue)
	log := d.o.log.With("session", sessionID, "target", d.cfg.Target, "address", address)
	c := newConn(cl.api, sessionID, address, log)
	c.resumeDelay = d.o.resumeDelay
	c.kms, c.target = cl.kms, d.cfg.Target
	if sessionID == "" || streamURL == "" || token == "" {
		// Best effort: the session is unusable either way.
		_ = c.terminate()
		return nil, fmt.Errorf("starting an SSM session on %s: StartSession returned no session, stream URL or token", d.cfg.Target)
	}
	log.Debug("started SSM session")
	if err := c.open(ctx, d.o.dialer, cl.signer, streamURL, token); err != nil {
		// abort ends the session, whatever state the channel got to.
		c.abort(err)
		return nil, fmt.Errorf("opening SSM session %s on %s to %s: %w", sessionID, d.cfg.Target, address, err)
	}
	return c, nil
}

// newClients builds the SSM API and, when credentials are at hand, what
// signs the data channel's WebSocket request.
func (o *options) newClients(ctx context.Context, cfg Config) (*clients, error) {
	if o.api != nil {
		return &clients{api: o.api, kms: o.kms}, nil
	}
	awsCfg, err := loadConfig(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration%s: %w", profileSuffix(cfg.Profile), err)
	}
	if awsCfg.Region == "" {
		return nil, fmt.Errorf("no AWS region for the SSM tunnel to %s; add ?region=<region> to the ssm:// URL or set a region in the AWS profile", cfg.Target)
	}
	if awsCfg.Credentials == nil {
		return nil, fmt.Errorf("no AWS credentials%s; configure the profile or set AWS_PROFILE", profileSuffix(cfg.Profile))
	}
	var ssmOpts []func(*ssm.Options)
	if o.endpoint != "" {
		endpoint := o.endpoint
		ssmOpts = append(ssmOpts, func(so *ssm.Options) { so.BaseEndpoint = &endpoint })
	}
	kmsAPI := o.kms
	if kmsAPI == nil {
		kmsAPI = kms.NewFromConfig(awsCfg)
	}
	return &clients{
		api:    ssm.NewFromConfig(awsCfg, ssmOpts...),
		kms:    kmsAPI,
		signer: &streamSigner{creds: awsCfg.Credentials, region: awsCfg.Region},
	}, nil
}

// loadConfig loads the shared AWS configuration the way the built-in EKS
// provider does: the named profile, or the default chain, with an explicit
// region taking precedence over the profile's.
func loadConfig(ctx context.Context, profile, region string) (awsv2.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	opts = append(opts, awsconfig.WithAppID("tug/"+version.String()))
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

func profileSuffix(profile string) string {
	if profile == "" {
		return ""
	}
	return fmt.Sprintf(" for profile %q", profile)
}
