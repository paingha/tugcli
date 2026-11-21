package tunnel

import (
	"context"
	"io"
	"net"
	"net/url"

	"github.com/paingha/tugcli/internal/tunnel/ssm"
)

func init() { builders["ssm"] = dialOnly(newSSM) }

// newSSM reads ssm://<instance>?region=..&profile=.. and opens Session
// Manager port-forwarding sessions through one ssm.Dialer, which loads the
// AWS profile once for the life of the pool.
func newSSM(u *url.URL, p *Pool) (DialFunc, io.Closer, error) {
	cfg, err := ssm.ParseURL(u)
	if err != nil {
		return nil, nil, err
	}
	d, err := ssm.NewDialer(cfg, ssm.WithLogger(p.log))
	if err != nil {
		return nil, nil, err
	}
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		return d.Dial(ctx, address)
	}
	return dial, d, nil
}
