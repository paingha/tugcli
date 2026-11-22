//go:build !unix

package provider

// stopOnCPULimit does nothing: only Unix systems limit a plugin's
// processor time.
func stopOnCPULimit() (stop func()) { return func() {} }
