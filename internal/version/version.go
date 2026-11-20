// Package version reports the build's version. The variables are set at link
// time by the release build, for example:
//
//	go build -ldflags "-X github.com/paingha/tugcli/internal/version.version=v0.1.0"
package version

import (
	"runtime"
	"runtime/debug"
)

// Set by -ldflags at release time.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build information, falling back to the module version and
// VCS details that the Go toolchain embeds.
func Get() Info {
	info := Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && info.Commit == "":
				info.Commit = s.Value
			case s.Key == "vcs.time" && info.Date == "":
				info.Date = s.Value
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	return info
}

// String returns the version alone.
func String() string { return Get().Version }
