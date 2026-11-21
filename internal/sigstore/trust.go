package sigstore

import (
	"context"
	_ "embed" // for the built-in trust root
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/util"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

//go:generate go run ./internal/fetchroot trusted_root.json

// builtinRoot is the public-good trusted root as it was when tug was built,
// fetched through TUF by go generate. sigstore-go builds in only the TUF
// root, which says who may sign the trust root but not what it is, so tug
// carries its own copy. It is used when the TUF repository cannot be reached
// and nothing usable is cached, as on a first run on a machine that can
// reach a plugin index but not sigstore.dev.
//
//go:embed trusted_root.json
var builtinRoot []byte

const (
	// trustedRootTarget is the TUF target that holds the trust root.
	trustedRootTarget = "trusted_root.json"
	// cacheValidityDays is how long cached TUF metadata is used before tug
	// asks the repository again.
	cacheValidityDays = 1
	// tufTimeout bounds each download from the TUF repository.
	tufTimeout = 20 * time.Second
)

// PublicGood returns a Verifier for the public-good Sigstore deployment
// that the Sigstore project runs, which is what cosign signs with by
// default.
//
// Its trust root comes from the Sigstore TUF repository, checked from the
// TUF root built into sigstore-go, and the TUF metadata is cached in
// cacheDir. tug asks the repository at most once a day. When it cannot be
// reached, the cached metadata serves until it expires, and after that, or
// when nothing is cached yet, the trust root built into tug serves instead;
// Stale then says why.
func PublicGood(ctx context.Context, cacheDir string) (*Verifier, error) {
	return publicGood(ctx, cacheDir, tuf.DefaultMirror)
}

// publicGood is PublicGood with the TUF repository at mirror.
func publicGood(ctx context.Context, cacheDir, mirror string) (*Verifier, error) {
	tr, err := fetchTrustedRoot(ctx, cacheDir, mirror, false)
	if err != nil && ctx.Err() == nil {
		// Offline: unexpired metadata in the cache still serves.
		if cached, cerr := fetchTrustedRoot(ctx, cacheDir, mirror, true); cerr == nil {
			tr, err = cached, nil
		}
	}
	if err == nil {
		return New(tr), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	builtin, berr := root.NewTrustedRootFromJSON(builtinRoot)
	if berr != nil {
		return nil, fmt.Errorf("reading the trust root built into tug: %w", berr)
	}
	return &Verifier{trusted: builtin, stale: err}, nil
}

// fetchTrustedRoot reads the trust root through the TUF repository at
// mirror. With cachedOnly it does not refresh metadata that is still valid.
func fetchTrustedRoot(ctx context.Context, cacheDir, mirror string, cachedOnly bool) (*root.TrustedRoot, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	opts := tuf.DefaultOptions().
		WithRepositoryBaseURL(mirror).
		WithCachePath(cacheDir).
		WithCacheValidity(cacheValidityDays).
		WithFetcher(contextFetcher(ctx)).
		WithContext(ctx)
	if cachedOnly {
		opts = opts.WithForceCache()
	}
	c, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("updating from %s: %w", opts.RepositoryBaseURL, err)
	}
	b, err := c.GetTarget(trustedRootTarget)
	if err != nil {
		return nil, fmt.Errorf("fetching %s from %s: %w", trustedRootTarget, opts.RepositoryBaseURL, err)
	}
	return root.NewTrustedRootFromJSON(b)
}

// contextFetcher is go-tuf's fetcher with each request bound to ctx and a
// timeout. go-tuf's own fetcher interface has no context, so without this a
// cancelled install would wait for an unreachable repository.
func contextFetcher(ctx context.Context) fetcher.Fetcher {
	client := &http.Client{Timeout: tufTimeout}
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPUserAgent(util.ConstructUserAgent())
	f.SetHTTPClient(doFunc(func(r *http.Request) (*http.Response, error) {
		return client.Do(r.WithContext(ctx))
	}))
	return f
}

// doFunc adapts a function to the HTTP client interface go-tuf takes.
type doFunc func(*http.Request) (*http.Response, error)

// Do sends the request.
func (f doFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
