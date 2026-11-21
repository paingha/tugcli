package ovhcloud

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// signer signs OVHcloud API requests as OVHcloud's API guide and its own
// clients do: "$1$" and the hex SHA-1 of the application secret, consumer
// key, method, full URL, body and timestamp joined with "+", sent with the
// application key, consumer key and timestamp in X-Ovh-* headers. The
// timestamp is the API's clock, from /auth/time, not the local one.
type signer struct {
	appKey, appSecret, consumerKey string
	// delta is how far the local clock is ahead of the API's.
	delta time.Duration
	now   func() time.Time
}

// sign sets the X-Ovh-* headers on req.
func (s *signer) sign(req *http.Request) error {
	body, err := requestBody(req)
	if err != nil {
		return provider.Errorf(provider.Internal, "signing %s: %v", req.URL.Path, err)
	}
	ts := strconv.FormatInt(s.now().Add(-s.delta).Unix(), 10)
	// SHA-1 is what the API's signature scheme specifies.
	sum := sha1.Sum([]byte(strings.Join([]string{s.appSecret, s.consumerKey, req.Method, req.URL.String(), string(body), ts}, "+")))
	req.Header.Set("X-Ovh-Application", s.appKey)
	req.Header.Set("X-Ovh-Consumer", s.consumerKey)
	req.Header.Set("X-Ovh-Timestamp", ts)
	req.Header.Set("X-Ovh-Signature", "$1$"+hex.EncodeToString(sum[:]))
	return nil
}

// requestBody returns a copy of the request's body, which the transport
// still sends.
func requestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("the body of %s %s cannot be read twice", req.Method, req.URL.Path)
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}
