//go:build windows

package browser

func command(url string) (string, []string) {
	return "rundll32", []string{"url.dll,FileProtocolHandler", url}
}
