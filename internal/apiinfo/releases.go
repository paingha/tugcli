package apiinfo

import (
	"cmp"
	_ "embed" // the upstream table is embedded
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/yaml"
)

//go:embed kubernetes.yaml
var upstreamYAML []byte

// Table is what tug knows of upstream Kubernetes releases: when each minor
// release's support ends and which API versions each release stopped
// serving. kubernetes.yaml says where it comes from.
type Table struct {
	// Checked is the day the table was last checked against its sources,
	// as YYYY-MM-DD.
	Checked string `json:"checked"`
	// Sources are the pages the table is copied from.
	Sources []string `json:"sources"`
	// Releases are the minor releases, newest first.
	Releases []Release `json:"releases"`
	// Removals are the API versions upstream releases stopped serving.
	Removals []Removal `json:"removals"`
}

// Release is one minor release of Kubernetes.
type Release struct {
	// Version is the minor release, such as "1.35".
	Version string `json:"version"`
	// EndOfLife is the day upstream support ends, as YYYY-MM-DD.
	EndOfLife string `json:"endOfLife"`
}

// Removal is an API version that a release of Kubernetes stopped serving
// for some kinds.
type Removal struct {
	GroupVersion string   `json:"groupVersion"`
	Kinds        []string `json:"kinds"`
	// RemovedIn is the first minor release that does not serve it.
	RemovedIn string `json:"removedIn"`
	// Replacement is the group-version to use instead, if there is one.
	Replacement string `json:"replacement,omitempty"`
	// ReplacementSince is the release the replacement was first served
	// in, when the migration guide says.
	ReplacementSince string `json:"replacementSince,omitempty"`
	// Note says what to do when there is no replacement API.
	Note string `json:"note,omitempty"`
}

var upstream = sync.OnceValue(func() *Table {
	t, err := parseTable(upstreamYAML)
	if err != nil {
		panic(fmt.Sprintf("apiinfo: the embedded kubernetes.yaml is invalid: %v", err))
	}
	return t
})

// Upstream returns the table embedded in tug.
func Upstream() *Table { return upstream() }

// parseTable reads a table and checks that its versions and dates parse.
func parseTable(b []byte) (*Table, error) {
	var t Table
	if err := yaml.UnmarshalStrict(b, &t); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.DateOnly, t.Checked); err != nil {
		return nil, fmt.Errorf("checked: %w", err)
	}
	for i, r := range t.Releases {
		if _, ok := parseMinor(r.Version); !ok {
			return nil, fmt.Errorf("release %q is not a minor version such as 1.35", r.Version)
		}
		if _, err := time.Parse(time.DateOnly, r.EndOfLife); err != nil {
			return nil, fmt.Errorf("release %s: endOfLife: %w", r.Version, err)
		}
		if i > 0 && compareMinor(t.Releases[i-1].Version, r.Version) <= 0 {
			return nil, fmt.Errorf("release %s is listed after %s; list the newest first", r.Version, t.Releases[i-1].Version)
		}
	}
	for _, r := range t.Removals {
		if _, ok := parseMinor(r.RemovedIn); !ok || len(r.Kinds) == 0 || r.GroupVersion == "" {
			return nil, fmt.Errorf("removal of %s %v in %q needs a group-version, kinds and a minor release", r.GroupVersion, r.Kinds, r.RemovedIn)
		}
		if r.Replacement == "" && r.Note == "" {
			return nil, fmt.Errorf("removal of %s %v needs a replacement or a note", r.GroupVersion, r.Kinds)
		}
	}
	return &t, nil
}

// Removal returns the upstream removal of a kind at a group-version, if
// upstream Kubernetes removes it.
func (t *Table) Removal(groupVersion, kind string) (Removal, bool) {
	for _, r := range t.Removals {
		if r.GroupVersion != groupVersion {
			continue
		}
		for _, k := range r.Kinds {
			if k == kind {
				return r, true
			}
		}
	}
	return Removal{}, false
}

// Newest returns the newest minor release in the table.
func (t *Table) Newest() string {
	if len(t.Releases) == 0 {
		return ""
	}
	return t.Releases[0].Version
}

// SupportStatus says where a release is in its upstream support window.
type SupportStatus string

// Support statuses.
const (
	// Supported releases get every upstream patch.
	Supported SupportStatus = "supported"
	// Maintenance releases are in their last two months of support and
	// get only critical fixes.
	Maintenance SupportStatus = "maintenance"
	// EndOfLife releases get no upstream patches.
	EndOfLife SupportStatus = "end-of-life"
	// Unknown releases are newer than the table, or not a version tug can
	// read.
	Unknown SupportStatus = "unknown"
)

// maintenanceMonths is how long before its end of life a release is in
// maintenance mode.
const maintenanceMonths = 2

// Support is where a release stands in its upstream support window on a
// given day.
type Support struct {
	Status SupportStatus `json:"status"`
	// EndOfLife is the day upstream support ends or ended, as YYYY-MM-DD,
	// when the table has the release.
	EndOfLife string `json:"endOfLife,omitempty"`
	// OldestSupported is the oldest release upstream still supports that
	// day, which is what to upgrade to at least.
	OldestSupported string `json:"oldestSupported,omitempty"`
	// Newest is the newest release in the table.
	Newest string `json:"newest,omitempty"`
	// Behind is how many minor releases the release is behind Newest.
	Behind int `json:"behind"`
	// TableChecked is the day the table was last checked.
	TableChecked string `json:"tableChecked"`
}

// Support returns where minor release v, such as "1.33", stands on day now.
func (t *Table) Support(v string, now time.Time) Support {
	s := Support{Status: Unknown, Newest: t.Newest(), TableChecked: t.Checked}
	for i := len(t.Releases) - 1; i >= 0; i-- {
		r := t.Releases[i]
		if eol, _ := time.Parse(time.DateOnly, r.EndOfLife); now.Before(eol) {
			s.OldestSupported = r.Version
			break
		}
	}
	if _, ok := parseMinor(v); !ok {
		return s
	}
	if s.Newest != "" {
		s.Behind = max(0, minorDistance(s.Newest, v))
	}
	for _, r := range t.Releases {
		if r.Version != v {
			continue
		}
		eol, _ := time.Parse(time.DateOnly, r.EndOfLife)
		s.EndOfLife = r.EndOfLife
		switch {
		case !now.Before(eol):
			s.Status = EndOfLife
		case !now.Before(eol.AddDate(0, -maintenanceMonths, 0)):
			s.Status = Maintenance
		default:
			s.Status = Supported
		}
		return s
	}
	// Releases older than the table's oldest ended long ago.
	if n := len(t.Releases); n > 0 && compareMinor(v, t.Releases[n-1].Version) < 0 {
		s.Status = EndOfLife
	}
	return s
}

// Server is a cluster's Kubernetes version.
type Server struct {
	// GitVersion is the version the server reports, such as
	// v1.35.1-eks-a1b2c3.
	GitVersion string `json:"gitVersion"`
	// Minor is its minor release, such as "1.35", or "" when tug cannot
	// read it.
	Minor    string `json:"minor"`
	Platform string `json:"platform,omitempty"`
}

var leadingMinor = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

var digits = regexp.MustCompile(`^\d+`)

// ServerOf reads the minor release from the version a server reports. The
// git version is the most reliable; some providers report a minor such as
// "35+".
func ServerOf(info *version.Info) Server {
	s := Server{GitVersion: info.GitVersion, Platform: info.Platform}
	if m := leadingMinor.FindStringSubmatch(info.GitVersion); m != nil {
		s.Minor = m[1] + "." + m[2]
		return s
	}
	major, minor := digits.FindString(info.Major), digits.FindString(info.Minor)
	if major != "" && minor != "" {
		s.Minor = major + "." + minor
	}
	return s
}

// minorVersion is a parsed minor release.
type minorVersion struct{ major, minor int }

func parseMinor(s string) (minorVersion, bool) {
	m := leadingMinor.FindStringSubmatch(s)
	if m == nil || m[0] != s {
		return minorVersion{}, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	return minorVersion{major, minor}, err1 == nil && err2 == nil
}

// compareMinor compares two minor releases, as -1, 0 or 1. A release that
// does not parse sorts first.
func compareMinor(a, b string) int {
	va, okA := parseMinor(a)
	vb, okB := parseMinor(b)
	switch {
	case !okA || !okB:
		return compareBool(okA, okB)
	case va.major != vb.major:
		return cmp.Compare(va.major, vb.major)
	default:
		return cmp.Compare(va.minor, vb.minor)
	}
}

// minorDistance is how many minor releases b is behind a, within one major
// version; 0 when either does not parse or the majors differ.
func minorDistance(a, b string) int {
	va, okA := parseMinor(a)
	vb, okB := parseMinor(b)
	if !okA || !okB || va.major != vb.major {
		return 0
	}
	return va.minor - vb.minor
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}

// AtLeast reports whether minor release v is release r or later.
func AtLeast(v, r string) bool {
	_, okV := parseMinor(v)
	_, okR := parseMinor(r)
	return okV && okR && compareMinor(v, r) >= 0
}
