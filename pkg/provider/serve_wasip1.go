//go:build wasip1

package provider

// serveInOrder makes serve answer one request before reading the next. A
// WebAssembly module under WASI preview 1 has one thread, and a read from
// stdin blocks all its goroutines until data arrives, so a reader running
// beside the requests would hold every answer back until the next line came.
const serveInOrder = true
