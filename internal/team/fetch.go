package team

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/paingha/tugcli/internal/config"
)

// Source is a team catalog tug follows: where it is published and the key
// it must be signed with.
type Source struct {
	Name      string
	URL       string
	Git       *config.TeamGit
	PublicKey ed25519.PublicKey
	// InConfig is set for a team listed in config.yaml rather than added
	// with tug team add.
	InConfig bool
}

// NewSource checks a configured team catalog and returns it as a Source.
func NewSource(t config.TeamCatalog, inConfig bool) (Source, error) {
	if !ValidName(t.Name) {
		return Source{}, fmt.Errorf("team name %q is not valid: use lowercase letters, digits and dashes", t.Name)
	}
	key, err := ParsePublicKey(t.PublicKey)
	if err != nil {
		return Source{}, fmt.Errorf("team %s: %w", t.Name, err)
	}
	src := Source{Name: t.Name, URL: t.URL, PublicKey: key, InConfig: inConfig}
	switch {
	case t.URL != "" && t.Git != nil:
		return Source{}, fmt.Errorf("team %s: set a url or a git repository, not both", t.Name)
	case t.URL != "":
		if !secureURL(t.URL) {
			return Source{}, fmt.Errorf("team %s: the catalog's url must be https", t.Name)
		}
		if hasPassword(t.URL) {
			return Source{}, fmt.Errorf("team %s: the catalog's url carries a password; tug keeps no secrets in its settings", t.Name)
		}
	case t.Git != nil:
		g := *t.Git
		if g.Path == "" {
			g.Path = DefaultGitPath
		}
		if err := checkGit(g); err != nil {
			return Source{}, fmt.Errorf("team %s: %w", t.Name, err)
		}
		src.Git = &g
	default:
		return Source{}, fmt.Errorf("team %s needs the catalog's url, or a git repository and path", t.Name)
	}
	return src, nil
}

// Config returns the source as config.yaml writes it.
func (s Source) Config(publicKey string) config.TeamCatalog {
	return config.TeamCatalog{Name: s.Name, URL: s.URL, Git: s.Git, PublicKey: publicKey}
}

// Location says where the catalog is fetched from.
func (s Source) Location() string {
	if s.Git == nil {
		return s.URL
	}
	loc := s.Git.Repo + " " + s.Git.Path
	if s.Git.Ref != "" {
		loc += "@" + s.Git.Ref
	}
	return loc
}

// checkGit refuses repositories and refs that git would read as options,
// and git's transports that run commands.
func checkGit(g config.TeamGit) error {
	switch {
	case g.Repo == "":
		return errors.New("the git repository is empty")
	case strings.HasPrefix(g.Repo, "-"), strings.HasPrefix(g.Ref, "-"):
		return errors.New("the git repository and ref cannot start with a dash")
	case strings.Contains(g.Repo, "::"):
		return fmt.Errorf("git transport in %s is not allowed: use an https or ssh URL, or a path", g.Repo)
	case strings.HasPrefix(strings.ToLower(g.Repo), "http://") && !secureURL(g.Repo):
		return errors.New("the git repository's URL must be https")
	case hasPassword(g.Repo):
		return errors.New("the git repository's URL carries a password; use your git credential helper or ssh agent instead")
	case !filepath.IsLocal(filepath.FromSlash(g.Path)):
		return fmt.Errorf("the catalog's path %s must be inside the repository", g.Path)
	}
	return nil
}

// Fetcher downloads team catalogs and their signatures.
type Fetcher struct {
	// HTTP is the client for https catalogs; a client with a 30 second
	// timeout when nil.
	HTTP *http.Client
	// Git is the git command, "git" when empty. It runs with the user's
	// own git credentials.
	Git string
	// WorkDir holds the shallow clones of git catalogs while they are
	// read.
	WorkDir string
}

// Fetch returns the catalog of src and its signature, unchecked.
func (f *Fetcher) Fetch(ctx context.Context, src Source) (data, sig []byte, err error) {
	if src.Git != nil {
		return f.fetchGit(ctx, src)
	}
	if data, err = f.get(ctx, src.URL); err != nil {
		return nil, nil, err
	}
	if sig, err = f.get(ctx, src.URL+SignatureSuffix); err != nil {
		return nil, nil, fmt.Errorf("the catalog's signature: %w", err)
	}
	return data, sig, nil
}

func (f *Fetcher) get(ctx context.Context, rawURL string) ([]byte, error) {
	if !secureURL(rawURL) {
		return nil, fmt.Errorf("refusing %s: team catalogs are downloaded over https only", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", rawURL, resp.Status)
	}
	return readSmall(resp.Body, rawURL)
}

func readSmall(r io.Reader, what string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCatalogBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", what, maxCatalogBytes>>20)
	}
	return b, nil
}

// fetchGit makes a shallow clone of the repository, reads the catalog and
// its signature, and removes the clone.
func (f *Fetcher) fetchGit(ctx context.Context, src Source) ([]byte, []byte, error) {
	g := src.Git
	work := f.WorkDir
	if work == "" {
		work = os.TempDir()
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(work, "team-"+src.Name+"-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	gitCmd := f.Git
	if gitCmd == "" {
		gitCmd = "git"
	}
	args := []string{"-c", "protocol.ext.allow=never", "-c", "protocol.fd.allow=never",
		"clone", "--quiet", "--depth=1", "--no-tags", "--single-branch"}
	if g.Ref != "" {
		args = append(args, "--branch="+g.Ref)
	}
	args = append(args, "--", g.Repo, filepath.Join(dir, "repo"))
	cmd := exec.CommandContext(ctx, gitCmd, args...)
	// Never stop to ask for a password: a person's git credentials come
	// from their credential helper or ssh agent, as for any clone.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, nil, fmt.Errorf("cloning %s: %s", g.Repo, msg)
		}
		return nil, nil, fmt.Errorf("cloning %s: %w", g.Repo, err)
	}
	path := filepath.Join(dir, "repo", filepath.FromSlash(g.Path))
	data, err := readFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s from %s: %w", g.Path, g.Repo, err)
	}
	sig, err := readFile(path + SignatureSuffix)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s%s from %s: %w", g.Path, SignatureSuffix, g.Repo, err)
	}
	return data, sig, nil
}

// readFile reads a regular file of at most maxCatalogBytes, refusing a
// link that could point outside the clone.
func readFile(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readSmall(f, filepath.Base(path))
}
