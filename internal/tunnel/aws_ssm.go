package tunnel

import (
	"context"
	"io"
	"net"
	"net/url"

	"github.com/paingha/tugcli/internal/tunnel/ssm"
)

func init() { builders["ssm"] = newSSM }

// newSSM reads ssm://<instance>?region=..&profile=.. and opens one Session
// Manager port-forwarding session per connection.
func newSSM(u *url.URL, p *Pool) (DialFunc, io.Closer, error) {
	cfg, err := ssm.ParseURL(u)
	if err != nil {
		return nil, nil, err
	}
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		return ssm.Dial(ctx, cfg, address, ssm.WithLogger(p.log))
	}
	return dial, nil, nil
}
