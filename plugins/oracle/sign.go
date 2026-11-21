package oracle

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// signer signs OCI API requests as
// https://docs.oracle.com/en-us/iaas/Content/API/Concepts/signingrequests.htm
// describes, which follows draft-cavage-http-signatures-08: an RSA-SHA256
// signature over the date, request target and host, and for requests with
// a body also its length, type and SHA-256, sent as
// `Signature version="1",keyId=...,algorithm="rsa-sha256",headers=...,signature=...`.
type signer struct {
	key *apiKey
	now func() time.Time
}

// sign sets the Date, x-content-sha256 and Authorization headers on req.
func (s *signer) sign(req *http.Request) error {
	date := s.now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	names := []string{"date", "(request-target)", "host"}
	values := []string{date, strings.ToLower(req.Method) + " " + req.URL.RequestURI(), host}
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		body, err := requestBody(req)
		if err != nil {
			return provider.Errorf(provider.Internal, "signing %s: %v", req.URL.Path, err)
		}
		sum := sha256.Sum256(body)
		digest := base64.StdEncoding.EncodeToString(sum[:])
		req.Header.Set("X-Content-Sha256", digest)
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		names = append(names, "content-length", "content-type", "x-content-sha256")
		values = append(values, strconv.Itoa(len(body)), req.Header.Get("Content-Type"), digest)
	}
	lines := make([]string, len(names))
	for i := range names {
		lines[i] = names[i] + ": " + values[i]
	}
	hashed := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	// PKCS #1 v1.5 signatures are deterministic; SignPKCS1v15 ignores its
	// random source.
	sig, err := rsa.SignPKCS1v15(nil, s.key.key, crypto.SHA256, hashed[:])
	if err != nil {
		return provider.Errorf(provider.Internal, "signing %s: %v", req.URL.Path, err)
	}
	req.Header.Set("Authorization", fmt.Sprintf(`Signature version="1",keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		s.key.keyID(), strings.Join(names, " "), base64.StdEncoding.EncodeToString(sig)))
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
