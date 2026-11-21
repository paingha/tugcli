package exoscale

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// requestLifetime is how long a signed request stays valid, as in
// Exoscale's own Go and Python clients.
const requestLifetime = 10 * time.Minute

// signer signs Exoscale API v2 requests as
// https://openapi-v2.exoscale.com/topic/topic-api-request-signature
// describes: an HMAC-SHA256, keyed with the API secret, of the method and
// path, the body, the values of the query parameters sorted by name, the
// signed header values (none) and the expiry, joined by newlines, sent as
// "EXO2-HMAC-SHA256 credential=<key>,signed-query-args=<names>,
// expires=<unix time>,signature=<base64>".
type signer struct {
	key    string
	secret []byte
	now    func() time.Time
}

// sign sets the Authorization header on req.
func (s *signer) sign(req *http.Request) error {
	body, err := requestBody(req)
	if err != nil {
		return provider.Errorf(provider.Internal, "signing %s: %v", req.URL.Path, err)
	}
	query := req.URL.Query()
	var names []string
	var values strings.Builder
	for name, vals := range query {
		// Only parameters with exactly one value are signed, as in the
		// reference implementations.
		if len(vals) == 1 {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		values.WriteString(query.Get(name))
	}
	expires := strconv.FormatInt(s.now().Add(requestLifetime).Unix(), 10)
	msg := strings.Join([]string{req.Method + " " + req.URL.EscapedPath(), string(body), values.String(), "", expires}, "\n")
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(msg))

	auth := "EXO2-HMAC-SHA256 credential=" + s.key
	if len(names) > 0 {
		auth += ",signed-query-args=" + strings.Join(names, ";")
	}
	auth += ",expires=" + expires + ",signature=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", auth)
	return nil
}

// requestBody returns the body of req without consuming it.
func requestBody(req *http.Request) ([]byte, error) {
	switch {
	case req.GetBody != nil:
		rc, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	case req.Body != nil && req.Body != http.NoBody:
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(b))
		return b, nil
	default:
		return nil, nil
	}
}
