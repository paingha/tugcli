package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// File names under a Store's directory.
const (
	addedFile    = "teams.yaml"
	catalogFile  = "catalog.yaml"
	stateFile    = "state.json"
	addedVersion = "tug.teams/v1alpha1"
)

// ErrRollback is returned for a catalog older than the version accepted.
var ErrRollback = errors.New("older than the version already accepted")

// Store keeps the teams added with tug team add and, for every team, the
// last good version of its catalog, so that names, labels and policies
// keep working offline. It lives under <data>/teams.
type Store struct {
	Dir string
	Now func() time.Time
}

// NewStore returns the store under a data directory.
func NewStore(dataDir string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{Dir: filepath.Join(dataDir, "teams"), Now: now}
}

type addedTeams struct {
	APIVersion string               `json:"apiVersion"`
	Teams      []config.TeamCatalog `json:"teams,omitempty"`
}

// Added returns the teams added with tug team add, in the order they were
// added.
func (s *Store) Added() ([]config.TeamCatalog, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, addedFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a addedTeams
	if err := yaml.UnmarshalStrict(b, &a); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(s.Dir, addedFile), err)
	}
	return a.Teams, nil
}

// Add records a team added with tug team add.
func (s *Store) Add(t config.TeamCatalog) error {
	teams, err := s.Added()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(teams, func(x config.TeamCatalog) bool { return x.Name == t.Name }) {
		return fmt.Errorf("team %s is already added", t.Name)
	}
	return s.saveAdded(append(teams, t))
}

// RemoveAdded forgets a team added with tug team add and reports whether
// there was one by that name.
func (s *Store) RemoveAdded(name string) (bool, error) {
	teams, err := s.Added()
	if err != nil {
		return false, err
	}
	n := len(teams)
	teams = slices.DeleteFunc(teams, func(x config.TeamCatalog) bool { return x.Name == name })
	if len(teams) == n {
		return false, nil
	}
	return true, s.saveAdded(teams)
}

func (s *Store) saveAdded(teams []config.TeamCatalog) error {
	b, err := yaml.Marshal(addedTeams{APIVersion: addedVersion, Teams: teams})
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(s.Dir, addedFile), b, 0o600)
}

// State is what the store remembers about a team besides its catalog.
type State struct {
	// Version and SHA256 are of the catalog last accepted. Version is kept
	// even when the cached catalog cannot be read, so that an older one is
	// still refused.
	Version    int64     `json:"version,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	AcceptedAt time.Time `json:"acceptedAt,omitzero"`
	// CheckedAt is when the catalog was last fetched and checked, and
	// LastError why the last attempt failed, if it did.
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	LastError string    `json:"lastError,omitempty"`
}

// Accepted is the last good version of a team's catalog.
type Accepted struct {
	Document *Document
	Data     []byte
	Sig      []byte
	State    State
}

func (s *Store) teamDir(name string) string { return filepath.Join(s.Dir, name) }

// State returns what the store remembers about a team; the zero State when
// nothing.
func (s *Store) State(name string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(s.teamDir(name), stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("reading the state of team %s: %w", name, err)
	}
	return st, nil
}

func (s *Store) saveState(name string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(s.teamDir(name), stateFile), b, 0o600)
}

// Accepted returns the last good version of src's catalog, checked against
// src's key again, or nil when there is none.
func (s *Store) Accepted(src Source) (*Accepted, error) {
	st, err := s.State(src.Name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.teamDir(src.Name), catalogFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(path + SignatureSuffix)
	if err != nil {
		return nil, fmt.Errorf("the saved catalog of team %s has no signature: %w", src.Name, err)
	}
	doc, err := Parse(data, sig, src.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("the saved catalog of team %s: %w", src.Name, err)
	}
	return &Accepted{Document: doc, Data: data, Sig: sig, State: st}, nil
}

// Accept saves a checked catalog as the team's last good version.
func (s *Store) Accept(name string, data, sig []byte, doc *Document) error {
	dir := s.teamDir(name)
	path := filepath.Join(dir, catalogFile)
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(path+SignatureSuffix, sig, 0o600); err != nil {
		return err
	}
	now := s.Now()
	return s.saveState(name, State{Version: doc.Version, SHA256: digest(data), AcceptedAt: now, CheckedAt: now})
}

// NoteCheck records that the team's catalog was fetched and checked, or why
// it could not be.
func (s *Store) NoteCheck(name string, checkErr error) error {
	st, err := s.State(name)
	if err != nil {
		return err
	}
	if checkErr != nil {
		st.LastError = checkErr.Error()
	} else {
		st.LastError, st.CheckedAt = "", s.Now()
	}
	return s.saveState(name, st)
}

// Forget removes everything the store keeps for a team, apart from its
// entry in the teams added with tug team add.
func (s *Store) Forget(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("team name %q is not valid", name)
	}
	return os.RemoveAll(s.teamDir(name))
}

// Known returns the teams the store has a catalog or state for.
func (s *Store) Known() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Update is a team's catalog as fetched and checked: its signature, its
// shape and its version against the one accepted before.
type Update struct {
	Source   Source
	Document *Document
	Data     []byte
	Sig      []byte
	// Previous is the version accepted before; nil on the first sync.
	Previous *Accepted
	// New is set when the catalog differs from Previous and needs
	// reviewing before it is accepted.
	New bool
}

// Check fetches src's catalog and checks it. A catalog with a bad
// signature, an invalid shape, a version lower than the accepted one, or
// the accepted version's number with other contents is refused. The last
// good version stays in place whatever happens.
func (s *Store) Check(ctx context.Context, f *Fetcher, src Source) (*Update, error) {
	st, err := s.State(src.Name)
	if err != nil {
		return nil, err
	}
	// A saved catalog that no longer verifies, for example after the
	// team's key changed, is replaced by the fetched one; st still holds
	// its version.
	prev, _ := s.Accepted(src)
	data, sig, err := f.Fetch(ctx, src)
	if err != nil {
		return nil, err
	}
	doc, err := Parse(data, sig, src.PublicKey)
	if err != nil {
		return nil, err
	}
	switch {
	case doc.Version < st.Version:
		return nil, fmt.Errorf("version %d of team %s's catalog is %w (%d); tug refuses it so an old catalog cannot replace a newer one",
			doc.Version, src.Name, ErrRollback, st.Version)
	case doc.Version == st.Version && st.SHA256 != "" && digest(data) != st.SHA256:
		return nil, fmt.Errorf("team %s changed its catalog without raising its version from %d; ask the team to publish it as version %d",
			src.Name, st.Version, st.Version+1)
	}
	u := &Update{Source: src, Document: doc, Data: data, Sig: sig, Previous: prev}
	u.New = prev == nil || digest(data) != digest(prev.Data)
	return u, nil
}
