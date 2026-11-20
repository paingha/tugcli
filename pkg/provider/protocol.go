package provider

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Operations, passed to the plugin as its only argument.
const (
	OpDescribe    = "describe"
	OpAccounts    = "accounts"
	OpList        = "list"
	OpCredential  = "credential"
	OpRotate      = "rotate"
	OpCheckAccess = "checkAccess"
	OpServe       = "serve"
)

// MaxMessageBytes caps a single request or response.
const MaxMessageBytes = 16 << 20

// Message kinds.
const (
	kindDescribeRequest     = "DescribeRequest"
	kindDescribeResponse    = "DescribeResponse"
	kindAccountsRequest     = "AccountsRequest"
	kindAccountsResponse    = "AccountsResponse"
	kindListRequest         = "ListRequest"
	kindListResponse        = "ListResponse"
	kindCredentialRequest   = "CredentialRequest"
	kindRotateRequest       = "RotateRequest"
	kindCredentialResponse  = "CredentialResponse"
	kindCheckAccessRequest  = "CheckAccessRequest"
	kindCheckAccessResponse = "CheckAccessResponse"
	kindError               = "Error"
)

// SupportedAPIVersions lists every protocol version this SDK can answer,
// newest first. tug picks the highest version both sides support.
func SupportedAPIVersions() []string { return []string{APIVersion} }

// IsKnownOp reports whether op is a protocol operation.
func IsKnownOp(op string) bool {
	switch op {
	case OpDescribe, OpAccounts, OpList, OpCredential, OpRotate, OpCheckAccess, OpServe:
		return true
	default:
		return false
	}
}

// Description is the answer to describe.
type Description struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Description says in a few words what the provider reaches, such as
	// "Akamai (Linode) Kubernetes Engine".
	Description  string       `json:"description,omitempty"`
	Protocols    []string     `json:"protocols"`
	Capabilities Capability   `json:"capabilities"`
	Secrets      []SecretSpec `json:"secrets,omitempty"`
}

// Wire envelopes. Every message carries apiVersion and kind next to its
// payload; they stay unexported so the public API is the payload types.
type (
	typeMeta struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	listRequestMsg struct {
		typeMeta
		ListRequest
	}
	credentialRequestMsg struct {
		typeMeta
		CredentialRequest
	}
	checkAccessRequestMsg struct {
		typeMeta
		AccessRequest
	}
	describeResponseMsg struct {
		typeMeta
		Description
	}
	accountsResponseMsg struct {
		typeMeta
		Accounts []Account `json:"accounts"`
	}
	listResponseMsg struct {
		typeMeta
		Clusters []Cluster `json:"clusters"`
	}
	credentialResponseMsg struct {
		typeMeta
		Credential Credential `json:"credential"`
	}
	checkAccessResponseMsg struct {
		typeMeta
		Findings []Finding `json:"findings"`
	}
	errorResponseMsg struct {
		typeMeta
		Error
	}
)

func meta(kind string) typeMeta { return typeMeta{APIVersion: APIVersion, Kind: kind} }

// EncodeRequest returns the request message a host writes to a plugin's stdin
// for op. payload must be nil for describe and accounts, a ListRequest for
// list, a CredentialRequest for credential and rotate, and an AccessRequest
// for checkAccess.
func EncodeRequest(op string, payload any) ([]byte, error) {
	var msg any
	switch op {
	case OpDescribe:
		msg = meta(kindDescribeRequest)
	case OpAccounts:
		msg = meta(kindAccountsRequest)
	case OpList:
		req, ok := payload.(ListRequest)
		if !ok {
			return nil, fmt.Errorf("list needs a ListRequest, got %T", payload)
		}
		msg = listRequestMsg{typeMeta: meta(kindListRequest), ListRequest: req}
	case OpCredential, OpRotate:
		req, ok := payload.(CredentialRequest)
		if !ok {
			return nil, fmt.Errorf("%s needs a CredentialRequest, got %T", op, payload)
		}
		kind := kindCredentialRequest
		if op == OpRotate {
			kind = kindRotateRequest
		}
		msg = credentialRequestMsg{typeMeta: meta(kind), CredentialRequest: req}
	case OpCheckAccess:
		req, ok := payload.(AccessRequest)
		if !ok {
			return nil, fmt.Errorf("checkAccess needs an AccessRequest, got %T", payload)
		}
		msg = checkAccessRequestMsg{typeMeta: meta(kindCheckAccessRequest), AccessRequest: req}
	default:
		return nil, fmt.Errorf("unknown operation %q", op)
	}
	return json.Marshal(msg)
}

// DecodeDescribe decodes a describe response.
func DecodeDescribe(data []byte) (Description, error) {
	var m describeResponseMsg
	err := decodeResponse(data, kindDescribeResponse, &m)
	return m.Description, err
}

// DecodeAccounts decodes an accounts response.
func DecodeAccounts(data []byte) ([]Account, error) {
	var m accountsResponseMsg
	err := decodeResponse(data, kindAccountsResponse, &m)
	return m.Accounts, err
}

// DecodeList decodes a list response.
func DecodeList(data []byte) ([]Cluster, error) {
	var m listResponseMsg
	err := decodeResponse(data, kindListResponse, &m)
	return m.Clusters, err
}

// DecodeCredential decodes a credential or rotate response and validates it.
func DecodeCredential(data []byte) (Credential, error) {
	var m credentialResponseMsg
	if err := decodeResponse(data, kindCredentialResponse, &m); err != nil {
		return Credential{}, err
	}
	if err := m.Credential.Validate(); err != nil {
		return Credential{}, Errorf(Internal, "plugin returned an invalid credential: %v", err)
	}
	return m.Credential, nil
}

// DecodeCheckAccess decodes a checkAccess response.
func DecodeCheckAccess(data []byte) ([]Finding, error) {
	var m checkAccessResponseMsg
	err := decodeResponse(data, kindCheckAccessResponse, &m)
	return m.Findings, err
}

// decodeResponse decodes data into out. An Error message is returned as an
// *Error.
func decodeResponse(data []byte, wantKind string, out any) error {
	var tm typeMeta
	if err := json.Unmarshal(data, &tm); err != nil {
		return Errorf(Internal, "response is not JSON: %v", err)
	}
	if tm.Kind == kindError {
		var e errorResponseMsg
		if err := json.Unmarshal(data, &e); err != nil {
			return Errorf(Internal, "malformed error response: %v", err)
		}
		if e.Code == "" {
			e.Code = Internal
		}
		return &e.Error
	}
	if !slices.Contains(SupportedAPIVersions(), tm.APIVersion) {
		return Errorf(Internal, "unsupported apiVersion %q", tm.APIVersion)
	}
	if tm.Kind != wantKind {
		return Errorf(Internal, "expected kind %s, got %q", wantKind, tm.Kind)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return Errorf(Internal, "malformed %s: %v", wantKind, err)
	}
	return nil
}
