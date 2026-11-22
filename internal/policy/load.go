package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/paingha/tugcli/internal/config"
)

// maxPackBytes bounds the size of a pack file.
const maxPackBytes = 1 << 20

// Loader reads the packs config.yaml names.
type Loader struct {
	// Dir is where relative paths are taken from: tug's config directory.
	Dir string
	// Cache keeps verified copies of the packs downloaded from URLs, named
	// by their sha256, so that each is downloaded once.
	Cache string
	// HTTP downloads packs; nil means a client with a 30 second timeout.
	HTTP *http.Client
}

// Load returns the built-in baseline pack followed by the packs srcs name,
// in their order. Every pack must load, and no two may share a name.
func (l *Loader) Load(ctx context.Context, srcs []config.PolicyPackSource) ([]*Pack, error) {
	packs := []*Pack{Baseline()}
	var errs []error
	for i, src := range srcs {
		p, err := l.load(ctx, src)
		if err != nil {
			errs = append(errs, fmt.Errorf("policy.packs[%d] in config.yaml: %w", i, err))
			continue
		}
		for _, q := range packs {
			if q.Name == p.Name {
				where := q.Source
				if q.Name == BaselineName {
					where = "tug's own"
				}
				err = fmt.Errorf("%s is called %s, as %s is; give it another name", p.Source, p.Name, where)
				break
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("policy.packs[%d] in config.yaml: %w", i, err))
			continue
		}
		packs = append(packs, p)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return packs, nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CheckSource checks a pack's source as config.yaml gives it, without
// reading the pack: a path, or a url pinned by its sha256.
func CheckSource(src config.PolicyPackSource) error {
	want := strings.ToLower(strings.TrimSpace(src.SHA256))
	switch {
	case want != "" && !sha256Pattern.MatchString(want):
		return fmt.Errorf("sha256 %q must be 64 hexadecimal digits", src.SHA256)
	case src.Path != "" && src.URL != "":
		return errors.New("give a path or a url, not both")
	case src.URL != "" && want == "":
		return fmt.Errorf("%s needs a sha256, which pins the pack so that a change to it is never run unseen", src.URL)
	case src.Path == "" && src.URL == "":
		return errors.New("give the pack's path, or its url and sha256")
	}
	if src.URL != "" {
		if u, err := url.Parse(src.URL); err != nil || (u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname()))) {
			return fmt.Errorf("refusing %s: packs are downloaded over https only", src.URL)
		}
	}
	return nil
}

func (l *Loader) load(ctx context.Context, src config.PolicyPackSource) (*Pack, error) {
	if err := CheckSource(src); err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(src.SHA256))
	switch {
	case src.Path != "":
		path := src.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(l.Dir, path)
		}
		data, err := readSmall(path)
		if err != nil {
			return nil, err
		}
		if err := verify(data, want, path); err != nil {
			return nil, err
		}
		return Parse(data, path)
	default:
		data, err := l.fetch(ctx, src.URL, want)
		if err != nil {
			return nil, err
		}
		return Parse(data, src.URL)
	}
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPackBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxPackBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", path, maxPackBytes>>20)
	}
	return data, nil
}

// verify checks data against the sha256 it is pinned to, if any.
func verify(data []byte, want, origin string) error {
	if want == "" {
		return nil
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("%s has sha256 %s, not the %s config.yaml pins; if the pack changed on purpose, read it and update the pin", origin, got, want)
	}
	return nil
}

// fetch returns the pack at rawURL whose sha256 is want, from the cache when
// it is there.
func (l *Loader) fetch(ctx context.Context, rawURL, want string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("refusing %s: packs are downloaded over https only", rawURL)
	}
	cached := ""
	if l.Cache != "" {
		cached = filepath.Join(l.Cache, want+".yaml")
		if data, err := readSmall(cached); err == nil && verify(data, want, cached) == nil {
			return data, nil
		}
	}
	data, err := l.download(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if err := verify(data, want, rawURL); err != nil {
		return nil, err
	}
	if cached != "" {
		_ = config.WriteFileAtomic(cached, data, 0o600) // a cache; a failed write only costs a download
	}
	return data, nil
}

func (l *Loader) download(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", rawURL, resp.Status)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, maxPackBytes+1)); err != nil {
		return nil, fmt.Errorf("downloading %s: %w", rawURL, err)
	}
	if buf.Len() > maxPackBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", rawURL, maxPackBytes>>20)
	}
	return buf.Bytes(), nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
