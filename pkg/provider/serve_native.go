//go:build !wasip1

package provider

// serveInOrder is false for native plugins, which read requests while
// answering earlier ones, and answer them concurrently.
const serveInOrder = false
