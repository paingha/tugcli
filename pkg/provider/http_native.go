//go:build !wasip1

package provider

import "net/http"

func platformTransport() http.RoundTripper { return http.DefaultTransport }
