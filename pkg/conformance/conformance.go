// Package conformance checks that a provider plugin follows the tug provider
// protocol. `tug plugin test` runs it against any plugin, and CI runs it
// against every built-in provider through the same process interface, so
// plugins and built-ins are held to one standard.
//
// It depends only on the standard library and pkg/provider, so plugin authors
// can call Run from their own tests.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// Status of one check.
type Status string

// Check outcomes.
const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Result is the outcome of one check.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report collects every result.
type Report struct {
	Results []Result `json:"results"`
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Results, func(x Result) bool { return x.Status == Fail })
}

// Target is how to start the plugin. The operation is appended to Args.
type Target struct {
	Command string
	Args    []string
	// Env is the plugin's whole environment. Nil inherits tug's.
	Env []string
	// Timeout per call; defaults to 30 seconds.
	Timeout time.Duration
}

// Options configure the run.
type Options struct {
	// Name is the provider name the manifest declares; describe must match.
	Name string
	// Account to list. Without one, checks that need real data are skipped
	// when the plugin answers InvalidConfig, NotFound or Unauthenticated.
	Account provider.Account
	// Secrets for the account.
	Secrets map[string]string
	// DeclaredSecrets from the manifest. When there are any, the plugin must
	// answer Unauthenticated to invalid values.
	DeclaredSecrets []provider.SecretSpec
	// Now defaults to time.Now; credential expiries must be after it.
	Now func() time.Time
}

// killGrace is how long a plugin may take to exit after SIGTERM.
const killGrace = 2 * time.Second

// Run runs every check and returns the report.
func Run(ctx context.Context, t Target, o Options) Report {
	r := &runner{target: t, opts: o, ctx: ctx}
	if r.opts.Now == nil {
		r.opts.Now = time.Now
	}
	desc := r.checkDescribe()
	r.checkUnknownOp()
	r.checkEmptyRequest()
	clusters, listed := r.checkList()
	if listed {
		r.checkListStable(clusters)
		r.checkCredential(clusters)
	} else {
		r.skip("list is stable across calls", "list did not succeed")
		r.skip("credential is valid and unexpired", "list did not succeed")
	}
	r.checkBadSecrets()
	r.checkAccounts(desc)
	r.checkCancellation()
	r.checkOnlyJSON()
	if desc.Capabilities.Has(provider.CapServe) {
		r.skip("serve answers like one-shot mode", "serve mode is not covered by this version of the suite")
	} else {
		r.skip("serve answers like one-shot mode", "plugin does not declare serve")
	}
	return Report{Results: r.results}
}

type call struct {
	op     string
	stdout []byte
	stderr []byte
	code   int
	err    error
}

type runner struct {
	target  Target
	opts    Options
	ctx     context.Context
	results []Result
	calls   []call
}

func (r *runner) add(name string, s Status, detail string, args ...any) {
	if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	r.results = append(r.results, Result{Name: name, Status: s, Detail: detail})
}

func (r *runner) pass(name, detail string, args ...any) { r.add(name, Pass, detail, args...) }
func (r *runner) fail(name, detail string, args ...any) { r.add(name, Fail, detail, args...) }
func (r *runner) skip(name, detail string, args ...any) { r.add(name, Skip, detail, args...) }

// run starts the plugin for op with the given stdin.
func (r *runner) run(op string, stdin []byte) call {
	timeout := r.target.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.target.Command, append(slices.Clone(r.target.Args), op)...)
	cmd.Env = r.target.Env
	cmd.Stdin = bytes.NewReader(stdin)
	newGroup(cmd)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	// Children that outlive the plugin may hold its pipes open; stop waiting
	// for them after the grace period.
	cmd.WaitDelay = killGrace
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	c := call{op: op, stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: exitCode(err), err: err}
	if ctx.Err() != nil {
		c.err = fmt.Errorf("%s timed out after %s", op, timeout)
	}
	r.calls = append(r.calls, c)
	return c
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// runOp encodes a request for op and runs the plugin with it.
func (r *runner) runOp(op string, payload any) call {
	b, err := provider.EncodeRequest(op, payload)
	if err != nil {
		return call{op: op, code: -1, err: fmt.Errorf("encoding %s request: %w", op, err)}
	}
	return r.run(op, b)
}

func (r *runner) checkDescribe() provider.Description {
	const name = "describe is valid"
	c := r.run(provider.OpDescribe, nil)
	if c.err != nil && c.code != 0 {
		r.fail(name, "exit %d: %v %s", c.code, c.err, firstLine(c.stderr))
		return provider.Description{}
	}
	d, err := provider.DecodeDescribe(c.stdout)
	switch {
	case err != nil:
		r.fail(name, "%v", err)
	case r.opts.Name != "" && d.Name != r.opts.Name:
		r.fail(name, "describe names %q but the manifest names %q", d.Name, r.opts.Name)
	case !slices.ContainsFunc(d.Protocols, func(p string) bool { return slices.Contains(provider.SupportedAPIVersions(), p) }):
		r.fail(name, "protocols %v include none of %v", d.Protocols, provider.SupportedAPIVersions())
	case !d.Capabilities.Has(provider.CapList | provider.CapCredential):
		r.fail(name, "capabilities %v must include list and credential", d.Capabilities)
	default:
		r.pass(name, "%s %s speaks %v", d.Name, d.Version, d.Protocols)
	}
	return d
}

func (r *runner) checkUnknownOp() {
	const name = "unknown operations are rejected"
	c := r.run("tug-conformance-unknown", nil)
	r.expectError(name, c, "")
}

func (r *runner) checkEmptyRequest() {
	const name = "an empty list request is rejected"
	c := r.run(provider.OpList, nil)
	r.expectError(name, c, "")
}

// expectError checks for a non-zero exit with an Error message, optionally of
// a given code.
func (r *runner) expectError(name string, c call, want provider.Code) {
	if c.code == 0 {
		r.fail(name, "exited 0")
		return
	}
	var e struct {
		Kind string        `json:"kind"`
		Code provider.Code `json:"code"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(c.stdout), &e); err != nil || e.Kind != "Error" || e.Code == "" {
		r.fail(name, "exit %d but stdout is not an Error message: %q", c.code, truncate(c.stdout))
		return
	}
	if want != "" && e.Code != want {
		r.fail(name, "got %s, want %s", e.Code, want)
		return
	}
	r.pass(name, "exit %d, %s", c.code, e.Code)
}

// needsAccount reports whether an error means the suite lacks a real account.
func needsAccount(err error) bool {
	switch provider.CodeOf(err) {
	case provider.InvalidConfig, provider.NotFound, provider.Unauthenticated:
		return true
	default:
		return false
	}
}

func (r *runner) list() ([]provider.Cluster, error) {
	c := r.runOp(provider.OpList, provider.ListRequest{Account: r.opts.Account, Secrets: r.opts.Secrets})
	if c.err != nil && c.code <= 0 {
		return nil, c.err
	}
	clusters, err := provider.DecodeList(c.stdout)
	if err == nil && c.code != 0 {
		err = fmt.Errorf("list answered but exited %d", c.code)
	}
	return clusters, err
}

func (r *runner) checkList() ([]provider.Cluster, bool) {
	const name = "list returns valid clusters"
	clusters, err := r.list()
	if err != nil {
		if r.opts.Account.ID == "" && needsAccount(err) {
			r.skip(name, "needs an account: %v (pass --account)", err)
			return nil, false
		}
		r.fail(name, "%v", err)
		return nil, false
	}
	seen := map[string]bool{}
	for i, c := range clusters {
		where := fmt.Sprintf("cluster %d (%s)", i, c.ID)
		switch {
		case c.ID == "":
			r.fail(name, "cluster %d has no id", i)
			return nil, false
		case seen[c.ID]:
			r.fail(name, "%s: duplicate id", where)
			return nil, false
		case c.Name == "":
			r.fail(name, "%s: no name", where)
			return nil, false
		case c.Endpoints.Public == "" && c.Endpoints.Private == "":
			r.fail(name, "%s: no endpoint", where)
			return nil, false
		}
		for _, ep := range []string{c.Endpoints.Public, c.Endpoints.Private} {
			if ep == "" {
				continue
			}
			if u, err := url.Parse(ep); err != nil || u.Scheme == "" || u.Host == "" {
				r.fail(name, "%s: endpoint %q is not a URL", where, ep)
				return nil, false
			}
		}
		if c.Credential != nil {
			if err := c.Credential.Validate(); err != nil {
				r.fail(name, "%s: inline credential: %v", where, err)
				return nil, false
			}
		}
		seen[c.ID] = true
	}
	r.pass(name, "%d clusters", len(clusters))
	return clusters, true
}

func (r *runner) checkListStable(first []provider.Cluster) {
	const name = "list is stable across calls"
	second, err := r.list()
	if err != nil {
		r.fail(name, "second list failed: %v", err)
		return
	}
	a, b := ids(first), ids(second)
	if !slices.Equal(a, b) {
		r.fail(name, "first list %v, second %v", a, b)
		return
	}
	r.pass(name, "same %d ids", len(a))
}

func ids(cs []provider.Cluster) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	sort.Strings(out)
	return out
}

func (r *runner) checkCredential(clusters []provider.Cluster) {
	const name = "credential is valid and unexpired"
	if len(clusters) == 0 {
		r.skip(name, "the account has no clusters")
		return
	}
	id := clusters[0].ID
	c := r.runOp(provider.OpCredential, provider.CredentialRequest{
		Account: r.opts.Account, ClusterID: id, Secrets: r.opts.Secrets,
	})
	cred, err := provider.DecodeCredential(c.stdout)
	switch {
	case err != nil:
		r.fail(name, "cluster %s: %v", id, err)
	case cred.Expired(r.opts.Now(), 0):
		r.fail(name, "cluster %s: credential expired at %s", id, cred.ExpiresAt)
	default:
		exp := "no expiry"
		if cred.ExpiresAt != nil {
			exp = "expires " + cred.ExpiresAt.Format(time.RFC3339)
		}
		r.pass(name, "cluster %s: %s, %s", id, cred.Type, exp)
	}
}

func (r *runner) checkBadSecrets() {
	const name = "invalid secrets give Unauthenticated"
	if len(r.opts.DeclaredSecrets) == 0 {
		r.skip(name, "the plugin declares no secrets")
		return
	}
	bad := make(map[string]string, len(r.opts.DeclaredSecrets))
	for _, s := range r.opts.DeclaredSecrets {
		bad[s.Name] = "tug-conformance-invalid"
	}
	c := r.runOp(provider.OpList, provider.ListRequest{Account: r.opts.Account, Secrets: bad})
	r.expectError(name, c, provider.Unauthenticated)
}

func (r *runner) checkAccounts(d provider.Description) {
	const name = "accounts is valid"
	if !d.Capabilities.Has(provider.CapAccounts) {
		r.skip(name, "the plugin does not declare accounts")
		return
	}
	c := r.runOp(provider.OpAccounts, nil)
	accts, err := provider.DecodeAccounts(c.stdout)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	for i, a := range accts {
		if a.ID == "" {
			r.fail(name, "account %d has no id", i)
			return
		}
	}
	r.pass(name, "%d accounts", len(accts))
}

// checkCancellation starts list without finishing the request, sends SIGTERM,
// and expects the plugin to exit within the grace period.
func (r *runner) checkCancellation() {
	const name = "SIGTERM stops the plugin promptly"
	if runtime.GOOS == "windows" {
		r.skip(name, "Windows has no SIGTERM; tug kills plugins there")
		return
	}
	cmd := exec.Command(r.target.Command, append(slices.Clone(r.target.Args), provider.OpList)...)
	cmd.Env = r.target.Env
	newGroup(cmd)
	// Stdout and stderr stay nil (the null device), so no copying goroutine
	// can keep Wait blocked on a child that holds the pipes.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		r.fail(name, "%v", err)
		return
	}
	defer killGroup(cmd)               // clean up anything left behind
	time.Sleep(200 * time.Millisecond) // let it start reading the request
	if err := terminate(cmd); err != nil {
		stdin.Close()
		r.fail(name, "sending SIGTERM: %v", err)
		return
	}
	done := make(chan struct{})
	go func() {
		// Any exit counts: the status after SIGTERM is up to the plugin.
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		stdin.Close()
		r.pass(name, "exited within %s", killGrace)
	case <-time.After(killGrace):
		killGroup(cmd)
		stdin.Close()
		<-done
		r.fail(name, "still running %s after SIGTERM", killGrace)
	}
}

// checkOnlyJSON verifies that every call wrote exactly one JSON object to
// stdout and nothing else.
func (r *runner) checkOnlyJSON() {
	const name = "stdout carries only one JSON object"
	for _, c := range r.calls {
		if err := singleJSONObject(c.stdout); err != nil {
			r.fail(name, "%s: %v", c.op, err)
			return
		}
	}
	r.pass(name, "%d calls", len(r.calls))
}

func singleJSONObject(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("not JSON: %q", truncate(b))
	}
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("not a JSON object: %q", truncate(b))
	}
	if rest := bytes.TrimSpace(b[dec.InputOffset():]); len(rest) > 0 {
		return fmt.Errorf("extra output after the response: %q", truncate(rest))
	}
	return nil
}

func truncate(b []byte) string {
	const limit = 120
	s := string(bytes.TrimSpace(b))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

func firstLine(b []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(b), []byte("\n"))
	return string(line)
}
