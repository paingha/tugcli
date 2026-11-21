// Package audit keeps a local, append-only log of what tug does on
// clusters' behalf: the writes it lets through (those of tug's own commands,
// of every client that goes through tugd's proxy, and of kubectl commands
// tug runs on direct sessions) and how each turned out, the writes its guard
// refuses, sessions connecting to and leaving clusters, and unlock windows
// opening, closing and running out.
//
// A write keeps three things apart: the intent (the command the user ran,
// as far as tug can tell), the request tug or tugd saw (method and path),
// and the outcome (the response status, or kubectl's exit code), which is
// recorded once the answer comes and joined to the write when it is read.
// An entry never holds a request body, a query string, a token, or a
// command-line value that could be a secret.
//
// Entries are JSON lines in a file under tug's data directory. Every tug
// process and tugd append to it under a file lock, and it is rotated at a
// size limit, keeping a few older files. The log is a record for the user,
// kept with the user's own permissions; it is not protected against them.
package audit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/filelock"
)

// Sources of an entry.
const (
	// SourceTug is one of tug's own commands: a write through its guard,
	// or a change to a session such as tug connect or tug unlock.
	SourceTug = "tug"
	// SourceProxy is any client, such as kubectl or helm, through tugd,
	// or tugd itself noticing that an unlock ran out.
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

// Kinds of entry.
const (
	// KindWrite is a write tug let through, recorded before it is sent.
	// Entries recorded before kinds existed have none and are writes.
	KindWrite = "write"
	// KindResult is the outcome of an earlier write, which it names by ID.
	// Entries joins it to that write and does not return it.
	KindResult = "result"
	// KindRefused is a write the guard refused.
	KindRefused = "refused"
	// KindSessionStart is a session connecting to a cluster, or a new
	// terminal starting out connected to its parent's.
	KindSessionStart = "session-start"
	// KindSessionEnd is a session leaving a cluster: disconnecting,
	// connecting to another, or ending with its terminal.
	KindSessionEnd = "session-end"
	// KindUnlock is tug unlock allowing writes on a read-only session
	// until Until.
	KindUnlock = "unlock"
	// KindLock is tug lock ending an unlock early.
	KindLock = "lock"
	// KindUnlockExpired is an unlock running out. Whichever tug command or
	// tugd notices first records it, timed when it ran out.
	KindUnlockExpired = "unlock-expired"
)

// Kinds lists every kind Entries can return, in the order a person would
// look for them.
var Kinds = []string{
	KindWrite, KindRefused, KindSessionStart, KindSessionEnd, KindUnlock, KindLock, KindUnlockExpired,
}

// Reasons a write was refused.
const (
	// ReasonReadOnly means the session was read-only and not unlocked.
	ReasonReadOnly = "read-only"
	// ReasonNoResponse is the reason of a result without a status: the
	// request was sent, or meant to be, and no answer came.
	ReasonNoResponse = "no response"
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

// Entry is one event. JSON field names are a stable interface: add fields,
// do not rename or remove them.
type Entry struct {
	Time time.Time `json:"time"`
	// Kind is what the entry records: KindWrite, KindRefused, KindResult,
	// KindSessionStart and so on.
	Kind string `json:"kind,omitempty"`
	// ID names a write, so that the KindResult entry recorded once it is
	// answered can refer to it.
	ID string `json:"id,omitempty"`
	// Source is what made the request or the change: SourceTug,
	// SourceProxy or SourceKubectl.
	Source    string `json:"source"`
	Cluster   string `json:"cluster"`
	ClusterID string `json:"clusterID,omitempty"`
	// Session is the tug session the entry came from.
	Session string `json:"session,omitempty"`
	// Namespace is the namespace of a write's API path, or the session's
	// namespace for a session entry or a kubectl command.
	Namespace string `json:"namespace,omitempty"`
	// ReadOnly and Proxy describe the session of a session entry: whether
	// it opened read-only, and whether it goes through tugd's proxy.
	ReadOnly bool `json:"readOnly,omitempty"`
	Proxy    bool `json:"proxy,omitempty"`
	// Until is when the writes a KindUnlock entry allows stop, and, for
	// KindLock and KindUnlockExpired, when the unlock they end was due to
	// stop.
	Until time.Time `json:"until,omitzero"`
	// Allowed says how a write was allowed: AllowedReadWrite,
	// AllowedUnlocked or AllowedConfirmed.
	Allowed string `json:"allowed,omitempty"`
	// Intent is the command the user ran, as far as tug can tell, without
	// its arguments: the tug command, such as "tug restart", or the one a
	// client names in its Kubectl-Command header, such as "kubectl delete".
	Intent string `json:"intent,omitempty"`
	// Method and Path are the API request tug or tugd saw for a
	// SourceTug or SourceProxy write. Path has no query string, which can
	// carry the command an exec runs.
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	// Command is the kubectl command line of a SourceKubectl write, as
	// RedactCommand leaves it.
	Command []string `json:"command,omitempty"`
	// Client is the User-Agent of a SourceProxy write's client.
	Client string `json:"client,omitempty"`
	// Status is the HTTP status a write was answered with, by the API
	// server or by tugd when it could not forward it. 0 means no answer
	// came, or none has been recorded yet.
	Status int `json:"status,omitempty"`
	// Exit is the exit code of a SourceKubectl write's kubectl.
	Exit *int `json:"exit,omitempty"`
	// Reason says why a write was refused (ReasonReadOnly), or why it got
	// no answer (ReasonNoResponse).
	Reason string `json:"reason,omitempty"`
}

// NewID returns a random ID for a write.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail
	return hex.EncodeToString(b)
}

// NamespaceOf returns the namespace an API path is in, or "" for a
// cluster-wide one such as a namespace itself.
func NamespaceOf(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "namespaces" && ((parts[0] == "api" && i == 2) || (parts[0] == "apis" && i == 3)) {
			return parts[i+1]
		}
	}
	return ""
}

// Filter chooses entries. Its zero value chooses every one.
type Filter struct {
	// Cluster keeps the entries of the cluster with this name or ID.
	Cluster string
	// Since keeps the entries made at or after it.
	Since time.Time
	// Session keeps the entries of the session with this ID.
	Session string
	// Namespace keeps the entries in this namespace.
	Namespace string
	// Kinds keeps the entries of these kinds.
	Kinds []string
}

func (f Filter) keep(e Entry) bool {
	switch {
	case f.Cluster != "" && e.Cluster != f.Cluster && e.ClusterID != f.Cluster,
		f.Session != "" && e.Session != f.Session,
		f.Namespace != "" && e.Namespace != f.Namespace,
		len(f.Kinds) > 0 && !slices.Contains(f.Kinds, e.Kind):
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
// rotated files. Each write carries the status, exit code or reason of its
// KindResult entry, which is not returned itself; a write without one was
// not answered, or tug stopped before it could say. Entries recorded before
// kinds and namespaces existed are given them. A line that cannot be read,
// such as one cut short by a crash, is skipped.
func (l *Log) Entries(ctx context.Context, f Filter) ([]Entry, error) {
	out := []Entry{}
	writes := map[string]int{} // write ID to its index in out
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
		if out, err = readEntries(path, f, out, writes); err != nil {
			return nil, err
		}
	}
	slices.Reverse(out)
	// Writers take turns, but their clocks are read before the lock.
	slices.SortStableFunc(out, func(a, b Entry) int { return b.Time.Compare(a.Time) })
	return out, nil
}

// readEntries appends the entries of one file that f keeps to out, and
// joins results to the writes in out that writes indexes by ID.
func readEntries(path string, f Filter, out []Entry, writes map[string]int) ([]Entry, error) {
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
		if e.Kind == KindResult {
			if i, ok := writes[e.ID]; ok {
				out[i].Status, out[i].Exit, out[i].Reason = e.Status, e.Exit, e.Reason
			}
			continue
		}
		normalise(&e)
		if !f.keep(e) {
			continue
		}
		if e.Kind == KindWrite && e.ID != "" {
			writes[e.ID] = len(out)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return out, nil
}

// normalise fills in what entries recorded before kinds and namespaces
// existed lack.
func normalise(e *Entry) {
	if e.Kind == "" {
		e.Kind = KindWrite
	}
	if e.Namespace == "" && e.Path != "" {
		e.Namespace = NamespaceOf(e.Path)
	}
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
