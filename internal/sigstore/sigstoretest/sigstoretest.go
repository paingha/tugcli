// Package sigstoretest runs a virtual Sigstore deployment for tests: its own
// Fulcio certificate authority, Rekor log and trust root, all in memory, so
// that signatures can be made and checked without the network. Only tests
// import it.
package sigstoretest

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
)

// Sigstore is a virtual Sigstore deployment.
type Sigstore struct {
	vs *ca.VirtualSigstore
}

// New starts a virtual deployment. Its keys are valid for an hour.
func New(t testing.TB) *Sigstore {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	return &Sigstore{vs: vs}
}

// TrustedRoot returns the deployment's trust root. It lists no CT logs,
// since the virtual Fulcio does not embed signed certificate timestamps.
func (s *Sigstore) TrustedRoot(t testing.TB) *root.TrustedRoot {
	t.Helper()
	// The virtual deployment keys its Rekor logs by hex ID but stores the
	// hex string itself as the ID. A trust root read from JSON holds the raw
	// ID, so convert it for the root to survive a round trip.
	logs := map[string]*root.TransparencyLog{}
	for id, l := range s.vs.RekorLogs() {
		raw, err := hex.DecodeString(id)
		if err != nil {
			t.Fatal(err)
		}
		c := *l
		c.ID = raw
		logs[id] = &c
	}
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01, s.vs.FulcioCertificateAuthorities(), nil, s.vs.TimestampingAuthorities(), logs)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// TrustedRootJSON returns the trust root as a trusted_root.json file holds
// it.
func (s *Sigstore) TrustedRootJSON(t testing.TB) []byte {
	t.Helper()
	b, err := s.TrustedRoot(t).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Sign signs artifact as the given certificate identity and OIDC issuer,
// logs the signature in the virtual Rekor and returns a Sigstore bundle in
// JSON, as cosign sign-blob --bundle writes one. The virtual Fulcio records
// the identity as an email address; identity checks compare the value only.
func (s *Sigstore) Sign(t testing.TB, identity, issuer string, artifact []byte) []byte {
	t.Helper()
	ent, err := s.vs.Sign(identity, issuer, artifact)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := ent.VerificationContent()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ent.SignatureContent()
	if err != nil {
		t.Fatal(err)
	}
	ms := sc.MessageSignatureContent()
	entries, err := ent.TlogEntries()
	if err != nil {
		t.Fatal(err)
	}

	// The virtual Rekor returns an entry without the promise and proof of
	// inclusion that a bundle carries, so add them.
	tle := entries[0].TransparencyLogEntry()
	tle.KindVersion = &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}
	set, err := s.vs.RekorSignPayload(tlog.RekorPayload{
		Body:           base64.StdEncoding.EncodeToString(tle.GetCanonicalizedBody()),
		IntegratedTime: tle.GetIntegratedTime(),
		LogIndex:       tle.GetLogIndex(),
		LogID:          hex.EncodeToString(tle.GetLogId().GetKeyId()),
	})
	if err != nil {
		t.Fatal(err)
	}
	tle.InclusionPromise = &protorekor.InclusionPromise{SignedEntryTimestamp: set}
	proof, err := s.vs.GetInclusionProof(tle.GetCanonicalizedBody())
	if err != nil {
		t.Fatal(err)
	}
	rootHash, err := hex.DecodeString(*proof.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([][]byte, 0, len(proof.Hashes))
	for _, h := range proof.Hashes {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, b)
	}
	tle.InclusionProof = &protorekor.InclusionProof{
		LogIndex:   *proof.LogIndex,
		RootHash:   rootHash,
		TreeSize:   *proof.TreeSize,
		Hashes:     hashes,
		Checkpoint: &protorekor.Checkpoint{Envelope: *proof.Checkpoint},
	}

	mediaType, err := bundle.MediaTypeString("v0.3")
	if err != nil {
		t.Fatal(err)
	}
	b, err := bundle.NewBundle(&protobundle.Bundle{
		MediaType: mediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: vc.Certificate().Raw},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{tle},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: ms.Digest()},
				Signature:     ms.Signature(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
