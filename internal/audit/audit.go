// Package audit keeps a local, append-only log of the writes tug lets
// through to clusters: those of tug's own commands, of every client that
// goes through tugd's proxy, and of kubectl commands tug runs on direct
// sessions. An entry says what was written, where, from which session, and
// how the write was allowed. It never holds a request body, a query string,
// a token, or a command-line value that could be a secret.
//
// Entries are JSON lines in a file under tug's data directory. Every tug
// process and tugd append to it under a file lock, and it is rotated at a
// size limit, keeping a few older files.
package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/filelock"
)

// Sources of a write.
const (
	// SourceTug is one of tug's own commands, through its guard.
	SourceTug = "tug"
	// SourceProxy is any client, such as kubectl or helm, through tugd.
	SourceProxy = "tugd"
	// SourceKubectl is kubectl run by tug on a session that does not go
	// through tugd, where tug sees only the command line.
	SourceKubectl = "kubectl"
)

// How a write was allowed.
const (
	// AllowedReadWrite means the session was not read-only.
	AllowedReadWrite = "read-write"
	// AllowedUnlocked means a read-only session was unlocked with tug unlock.
	AllowedUnlocked = "unlocked"
	// AllowedConfirmed means the user said yes when tug asked.
	AllowedConfirmed = "confirmed"
)

// Defaults for a Log.
const (
	// DefaultMaxSize is the size at which the log is rotated.
	DefaultMaxSize = 8 << 20
	// DefaultKeep is how many rotated files are kept.
	DefaultKeep = 3
)

const (
	fileName = "audit.jsonl"
	lockName = "audit.lock"
	// lockPoll is how often a writer waiting for the lock tries again.
	lockPoll = 5 * time.Millisecond
	// maxLine bounds the length of a line that is read back.
	maxLine = 1 << 20
)

// Entry is one write. JSON field names are a stable interface: add fields,
// do not rename or remove them.
type Entry struct {
	Time time.Time `json:"time"`
	// Source is what made the write: SourceTug, SourceProxy or
	// SourceKubectl.
	Source    string `json:"source"`
	Cluster   string `json:"cluster"`
	ClusterID string `json:"clusterID,omitempty"`
	// Session is the tug session the write came from.
	Session string `json:"session,omitempty"`
	// Allowed says how the write was allowed: AllowedReadWrite,
	// AllowedUnlocked or AllowedConfirmed.
	Allowed string `json:"allowed"`
	// Method and Path are the API request of SourceTug and SourceProxy
	// writes. Path has no query string, which can carry the command an
	// exec runs.
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	// Command is the kubectl command line of a SourceKubectl write, as
	// RedactCommand leaves it.
	Command []string `json:"command,omitempty"`
	// Client is the User-Agent of a SourceProxy write's client.
	Client string `json:"client,omitempty"`
}

// Filter chooses entries. Its zero value chooses every one.
type Filter struct {
	// Cluster keeps the entries of the cluster with this name or ID.
	Cluster string
	// Since keeps the entries made at or after it.
	Since time.Time
}

func (f Filter) keep(e Entry) bool {
	if f.Cluster != "" && e.Cluster != f.Cluster && e.ClusterID != f.Cluster {
		return false
	}
	return f.Since.IsZero() || !e.Time.Before(f.Since)
}

// Log is the audit log in one directory.
type Log struct {
	dir     string
	maxSize int64
	keep    int
	now     func() time.Time
}

// Option configures a Log.
type Option func(*Log)

// WithMaxSize sets the size, in bytes, at which the log is rotated.
func WithMaxSize(n int64) Option { return func(l *Log) { l.maxSize = n } }

// WithKeep sets how many rotated files are kept, at least one.
func WithKeep(n int) Option { return func(l *Log) { l.keep = max(n, 1) } }

// WithNow sets the clock that times entries recorded without a time.
func WithNow(now func() time.Time) Option { return func(l *Log) { l.now = now } }

// Dir is the audit log's directory under tug's data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, "audit") }

// New returns the log kept in dir. Nothing is created until an entry is
// recorded.
func New(dir string, opts ...Option) *Log {
	l := &Log{dir: dir, maxSize: DefaultMaxSize, keep: DefaultKeep, now: time.Now}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Path is the file entries are appended to.
func (l *Log) Path() string { return filepath.Join(l.dir, fileName) }

// rotated is the path of the n-th older file, 1 being the newest.
func (l *Log) rotated(n int) string { return l.Path() + "." + strconv.Itoa(n) }

// Record appends e, timed now when it has no time. It waits for other
// writers until ctx ends.
func (l *Log) Record(ctx context.Context, e Entry) error {
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding an audit entry: %w", err)
	}
	line = append(line, '\n')
	if err := config.EnsurePrivateDir(l.dir); err != nil {
		return err
	}
	unlock, err := l.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := l.rotate(int64(len(line))); err != nil {
		return fmt.Errorf("rotating %s: %w", l.Path(), err)
	}
	f, err := os.OpenFile(l.Path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", l.Path(), err)
	}
	return f.Close()
}

// rotate moves the log aside when adding next bytes would take it past the
// size limit, dropping the oldest rotated file.
func (l *Log) rotate(next int64) error {
	fi, err := os.Stat(l.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Size() == 0 || fi.Size()+next <= l.maxSize {
		return nil
	}
	if err := os.Remove(l.rotated(l.keep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for n := l.keep - 1; n >= 1; n-- {
		if err := os.Rename(l.rotated(n), l.rotated(n+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.Rename(l.Path(), l.rotated(1))
}

// Entries returns the entries f keeps, newest first, from the log and its
// rotated files. A line that cannot be read, such as one cut short by a
// crash, is skipped.
func (l *Log) Entries(ctx context.Context, f Filter) ([]Entry, error) {
	out := []Entry{}
	if _, err := os.Stat(l.dir); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	unlock, err := l.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	for n := l.keep; n >= 0; n-- { // oldest first
		path := l.Path()
		if n > 0 {
			path = l.rotated(n)
		}
		if out, err = readEntries(path, f, out); err != nil {
			return nil, err
		}
	}
	slices.Reverse(out)
	// Writers take turns, but their clocks are read before the lock.
	slices.SortStableFunc(out, func(a, b Entry) int { return b.Time.Compare(a.Time) })
	return out, nil
}

// readEntries appends the entries of one file that f keeps to out.
func readEntries(path string, f Filter, out []Entry) ([]Entry, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if f.keep(e) {
			out = append(out, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return out, nil
}

// lock takes the log's lock, waiting for another writer until ctx ends.
func (l *Log) lock(ctx context.Context) (func(), error) {
	path := filepath.Join(l.dir, lockName)
	for {
		unlock, err := filelock.TryLock(path)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s: %w", path, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}
