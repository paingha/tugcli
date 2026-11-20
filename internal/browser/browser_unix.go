//go:build !windows

package browser

import "runtime"

func command(url string) (string, []string) {
	if runtime.GOOS == "darwin" {
		return "open", []string{url}
	}
	return "xdg-open", []string{url}
}
