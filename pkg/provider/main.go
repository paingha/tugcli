package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"
)

// Exit codes returned by Main and Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Provider is what a plugin implements. Optional operations are separate
// interfaces: AccountLister, Rotator and AccessChecker.
type Provider interface {
	// Info returns the name, version and capabilities. CapList and
	// CapCredential are implied.
	Info() Info
	// List returns the clusters in one account.
	List(ctx context.Context, req ListRequest) ([]Cluster, error)
	// Credential returns a credential for one cluster.
	Credential(ctx context.Context, req CredentialRequest) (Credential, error)
}

// AccountLister finds accounts already set up on this machine, for example
// profiles in a vendor CLI's config.
type AccountLister interface {
	Accounts(ctx context.Context) ([]Account, error)
}

// Rotator issues a new credential and revokes the old one where possible.
type Rotator interface {
	Rotate(ctx context.Context, req CredentialRequest) (Credential, error)
}

// AccessChecker explains why the caller may not reach a cluster, for example
// an IP allow list on the control plane.
type AccessChecker interface {
	CheckAccess(ctx context.Context, req AccessRequest) ([]Finding, error)
}

// Capabilities returns p's capabilities, including the ones implied by the
// optional interfaces it implements.
func Capabilities(p Provider) Capability {
	// Every SDK plugin can serve; the host uses it when the manifest says so.
	c := p.Info().Capabilities | CapList | CapCredential | CapServe
	if _, ok := p.(AccountLister); ok {
		c |= CapAccounts
	}
	if _, ok := p.(Rotator); ok {
		c |= CapRotate
	}
	if _, ok := p.(AccessChecker); ok {
		c |= CapCheckAccess
	}
	return c
}

// Main runs p as a plugin process using os.Args, stdin and stdout, and returns
// the exit code. SIGINT and SIGTERM cancel the context passed to p, and on
// Unix, SIGXCPU, which says the plugin has used up the processor time tug
// allowed it, ends the process. A plugin's main function is typically:
//
//	func main() { os.Exit(provider.Main(myProvider{})) }
func Main(p Provider) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer stopOnCPULimit()()
	return Run(ctx, p, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// Run is Main with explicit arguments and streams, for tests and for hosts that
// embed a provider.
func Run(ctx context.Context, p Provider, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: %s <describe|accounts|list|credential|rotate|checkAccess|serve>\n", progName())
		return ExitUsage
	}
	op := args[0]
	if !IsKnownOp(op) {
		writeError(stdout, stderr, op, Errorf(InvalidConfig, "unknown operation %q", op))
		return ExitUsage
	}
	if op == OpServe {
		return serve(ctx, p, stdin, stdout, stderr)
	}
	resp, err := dispatch(ctx, p, op, stdin)
	if err != nil {
		writeError(stdout, stderr, op, AsError(err))
		return ExitError
	}
	if err := writeJSON(stdout, resp); err != nil {
		fmt.Fprintf(stderr, "writing response: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func writeError(stdout, stderr io.Writer, op string, e *Error) {
	if e.Code == "" {
		e.Code = Internal
	}
	if err := writeJSON(stdout, errorResponseMsg{typeMeta: meta(kindError), Error: *e}); err != nil {
		fmt.Fprintf(stderr, "writing error response: %v\n", err)
	}
	fmt.Fprintf(stderr, "%s %s: %v\n", progName(), op, e)
}

func dispatch(ctx context.Context, p Provider, op string, stdin io.Reader) (any, error) {
	switch op {
	case OpDescribe:
		info := p.Info()
		return describeResponseMsg{
			typeMeta: meta(kindDescribeResponse),
			Description: Description{
				Name:         info.Name,
				Version:      info.Version,
				Description:  info.Description,
				Protocols:    SupportedAPIVersions(),
				Capabilities: Capabilities(p),
				Secrets:      info.Secrets,
			},
		}, nil

	case OpAccounts:
		al, ok := p.(AccountLister)
		if !ok {
			return nil, Errorf(Unimplemented, "accounts is not supported")
		}
		if _, err := readRequest[typeMeta](ctx, stdin, kindAccountsRequest, true); err != nil {
			return nil, err
		}
		accts, err := al.Accounts(ctx)
		if err != nil {
			return nil, err
		}
		return accountsResponseMsg{typeMeta: meta(kindAccountsResponse), Accounts: nonNil(accts)}, nil

	case OpList:
		req, err := readRequest[listRequestMsg](ctx, stdin, kindListRequest, false)
		if err != nil {
			return nil, err
		}
		clusters, err := p.List(ctx, req.ListRequest)
		if err != nil {
			return nil, err
		}
		return listResponseMsg{typeMeta: meta(kindListResponse), Clusters: nonNil(clusters)}, nil

	case OpCredential:
		req, err := readRequest[credentialRequestMsg](ctx, stdin, kindCredentialRequest, false)
		if err != nil {
			return nil, err
		}
		return credentialResponse(p.Credential(ctx, req.CredentialRequest))

	case OpRotate:
		r, ok := p.(Rotator)
		if !ok {
			return nil, Errorf(Unimplemented, "rotate is not supported")
		}
		req, err := readRequest[credentialRequestMsg](ctx, stdin, kindRotateRequest, false)
		if err != nil {
			return nil, err
		}
		return credentialResponse(r.Rotate(ctx, req.CredentialRequest))

	case OpCheckAccess:
		ac, ok := p.(AccessChecker)
		if !ok {
			return nil, Errorf(Unimplemented, "checkAccess is not supported")
		}
		req, err := readRequest[checkAccessRequestMsg](ctx, stdin, kindCheckAccessRequest, false)
		if err != nil {
			return nil, err
		}
		findings, err := ac.CheckAccess(ctx, req.AccessRequest)
		if err != nil {
			return nil, err
		}
		return checkAccessResponseMsg{typeMeta: meta(kindCheckAccessResponse), Findings: nonNil(findings)}, nil

	case OpServe:
		return nil, Errorf(InvalidConfig, "serve is a mode, not a request")

	default:
		return nil, Errorf(InvalidConfig, "unknown operation %q", op)
	}
}

func credentialResponse(cred Credential, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if err := cred.Validate(); err != nil {
		return nil, Errorf(Internal, "provider returned an invalid credential: %v", err)
	}
	return credentialResponseMsg{typeMeta: meta(kindCredentialResponse), Credential: cred}, nil
}

// readRequest reads and checks one request message. When optional is true an
// empty stdin is accepted. Reading stops when ctx ends, so a plugin told to
// stop while the host is still writing exits promptly.
func readRequest[T any](ctx context.Context, r io.Reader, kind string, optional bool) (T, error) {
	var zero T
	data, err := readAll(ctx, r)
	if err != nil {
		return zero, err
	}
	if len(data) > MaxMessageBytes {
		return zero, Errorf(InvalidConfig, "request exceeds %d bytes", MaxMessageBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if optional {
			return zero, nil
		}
		return zero, Errorf(InvalidConfig, "empty request; expected %s on stdin", kind)
	}
	var tm typeMeta
	if err := json.Unmarshal(data, &tm); err != nil {
		return zero, Errorf(InvalidConfig, "request is not JSON: %v", err)
	}
	if tm.APIVersion != "" && !slices.Contains(SupportedAPIVersions(), tm.APIVersion) {
		return zero, Errorf(InvalidConfig, "unsupported apiVersion %q; this plugin speaks %v", tm.APIVersion, SupportedAPIVersions())
	}
	if tm.Kind != "" && tm.Kind != kind {
		return zero, Errorf(InvalidConfig, "expected kind %s, got %q", kind, tm.Kind)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return zero, Errorf(InvalidConfig, "malformed %s: %v", kind, err)
	}
	return out, nil
}

// readAll reads r up to MaxMessageBytes+1 bytes unless ctx ends first. On
// cancellation the reading goroutine is left blocked on r; the process is
// about to exit, so it is not waited for.
func readAll(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(r, MaxMessageBytes+1))
		done <- result{data, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return nil, Errorf(Internal, "reading request: %v", res.err)
		}
		return res.data, nil
	case <-ctx.Done():
		return nil, Errorf(Unavailable, "cancelled while reading the request: %v", context.Cause(ctx))
	}
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func progName() string {
	if len(os.Args) > 0 {
		return os.Args[0]
	}
	return "tug-provider"
}
