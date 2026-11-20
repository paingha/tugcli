//go:build windows

package plugin

import "os"

// terminate stops a plugin. Windows has no SIGTERM, so it is killed.
func terminate(p *os.Process) error { return p.Kill() }
