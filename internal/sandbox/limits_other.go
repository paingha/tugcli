//go:build !linux && !darwin

package sandbox

import "runtime"

var limitSupport = LimitSupport{Missing: "processor time and memory: " + runtime.GOOS + " has no limits for plugins; they rely on timeouts"}
