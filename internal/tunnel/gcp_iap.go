package tunnel

import (
	"context"
	"io"
	"net"
	"net/url"
	"sync"

	"golang.org/x/oauth2"

	"github.com/paingha/tugcli/internal/tunnel/iap"
)

func init() { builders["iap"] = dialOnly(newIAP) }

// newIAP reads iap://<instance>?project=..&zone=.. or
// iap://?project=..&region=..&network=..&group=.. and opens one IAP TCP
// forwarding connection per connection, sharing one Google token source.
func newIAP(u *url.URL, p *Pool) (DialFunc, io.Closer, error) {
	cfg, err := iap.ParseURL(u)
	if err != nil {
		return nil, nil, err
	}
	var (
		once  sync.Once
		ts    oauth2.TokenSource
		tsErr error
	)
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		once.Do(func() { ts, tsErr = iap.DefaultTokenSource(ctx) })
		if tsErr != nil {
			return nil, tsErr
		}
		return iap.Dial(ctx, cfg, address, iap.WithTokenSource(ts), iap.WithLogger(p.log))
	}
	return dial, nil, nil
}
