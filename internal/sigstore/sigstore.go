// Package sigstore checks Sigstore signatures on plugin archives. A
// signature is good when its signing certificate chains to a trusted Fulcio
// certificate authority, its entry is proven to be in a trusted Rekor
// transparency log, and the certificate names the signer the caller expects,
// such as the release workflow in a publisher's GitHub repository.
package sigstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// GitHubIssuer is the OIDC issuer of GitHub Actions workflows, which sign
// keyless releases.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// ErrIdentityMismatch is returned when a bundle is valid but was signed by
// someone other than the expected signer.
var ErrIdentityMismatch = errors.New("the signer is not the one expected")

// Identity is who must have signed an archive.
type Identity struct {
	// Subject is the signing certificate's subject alternative name. For a
	// GitHub Actions workflow it is the workflow's URL and ref, such as
	// https://github.com/acme/tug-provider-acme/.github/workflows/release.yml@refs/tags/v0.1.0.
	Subject string
	// SubjectRegexp matches the subject instead, for example to accept every
	// tag of one workflow. It must match the whole subject.
	SubjectRegexp string
	// Issuer is the OIDC issuer that vouched for the subject, such as
	// GitHubIssuer.
	Issuer string
}

// Validate checks that the identity names exactly one of a subject and a
// subject regexp, that the regexp compiles, and that it names an issuer.
func (id Identity) Validate() error {
	var errs []error
	switch {
	case id.Subject == "" && id.SubjectRegexp == "":
		errs = append(errs, errors.New("a signer identity or identity regexp is needed"))
	case id.Subject != "" && id.SubjectRegexp != "":
		errs = append(errs, errors.New("give a signer identity or an identity regexp, not both"))
	case id.SubjectRegexp != "":
		if _, err := regexp.Compile(anchor(id.SubjectRegexp)); err != nil {
			errs = append(errs, fmt.Errorf("identity regexp: %w", err))
		}
	}
	if id.Issuer == "" {
		errs = append(errs, errors.New("an OIDC issuer for the signer is needed"))
	}
	return errors.Join(errs...)
}

// String describes the identity as a person reads it.
func (id Identity) String() string {
	subject := id.Subject
	if id.SubjectRegexp != "" {
		subject = "an identity matching " + id.SubjectRegexp
	}
	return subject + " (issuer " + id.Issuer + ")"
}

// anchor makes a regexp match whole strings only, so that a pattern for one
// repository cannot also match a longer identity that merely contains it.
func anchor(re string) string { return "^(?:" + re + ")$" }

// Signer is who signed a verified archive, and when.
type Signer struct {
	// Subject is the signing certificate's subject alternative name.
	Subject string
	// Issuer is the OIDC issuer that vouched for it.
	Issuer string
	// Time is when the transparency log or a timestamp authority saw the
	// signature.
	Time time.Time
}

// Verifier checks Sigstore bundles against a trust root.
type Verifier struct {
	trusted root.TrustedMaterial
	// stale is why the trust root built into tug is used instead of one
	// fresh from TUF; nil when it is not.
	stale error
}

// New returns a Verifier that trusts tm, such as a private Sigstore
// deployment's trust root.
func New(tm root.TrustedMaterial) *Verifier { return &Verifier{trusted: tm} }

// FromFile returns a Verifier that trusts the trusted_root.json at path, for
// a private Sigstore deployment or a machine that cannot reach TUF.
func FromFile(path string) (*Verifier, error) {
	tr, err := root.NewTrustedRootFromPath(path)
	if err != nil {
		return nil, fmt.Errorf("reading the Sigstore trusted root %s: %w", path, err)
	}
	return New(tr), nil
}

// Stale says why the Verifier uses the trust root built into tug rather than
// one fresh from TUF, or returns nil when it does not.
func (v *Verifier) Stale() error { return v.stale }

// Verify checks that bundleJSON, a Sigstore bundle, signs the file at path
// and was made by id. It checks the certificate chain, the transparency log
// entry and its inclusion proof, the time of signing, the signature over the
// file and, last, the signer's identity.
func (v *Verifier) Verify(path string, bundleJSON []byte, id Identity) (*Signer, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	b := &bundle.Bundle{}
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return nil, fmt.Errorf("reading the Sigstore bundle: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return v.verify(b, f, id)
}

func (v *Verifier) verify(entity verify.SignedEntity, artifact io.Reader, id Identity) (*Signer, error) {
	if !entity.HasInclusionProof() {
		// A signed promise of inclusion is not proof of it, and current
		// Sigstore bundles carry the proof.
		return nil, errors.New("the Sigstore bundle has no transparency log inclusion proof; sign with cosign 3, or cosign 2 with --new-bundle-format")
	}
	opts := []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	// Fulcio embeds signed certificate timestamps from the CT logs that
	// the trust root lists. A trust root without CT logs, such as a private
	// deployment's, cannot check them.
	if len(v.trusted.CTLogs()) > 0 {
		opts = append(opts, verify.WithSignedCertificateTimestamps(1))
	}
	sv, err := verify.NewVerifier(v.trusted, opts...)
	if err != nil {
		return nil, err
	}
	regex := ""
	if id.SubjectRegexp != "" {
		regex = anchor(id.SubjectRegexp)
	}
	want, err := verify.NewShortCertificateIdentity(id.Issuer, "", id.Subject, regex)
	if err != nil {
		return nil, err
	}
	res, err := sv.Verify(entity, verify.NewPolicy(verify.WithArtifact(artifact), verify.WithCertificateIdentity(want)))
	var mismatch *verify.ErrNoMatchingCertificateIdentity
	if errors.As(err, &mismatch) {
		// Identities are checked last, so everything else about the
		// signature is sound and the certificate names its real signer.
		if got, gerr := signerOf(entity); gerr == nil {
			return nil, fmt.Errorf("%w: signed by %s, expected %s", ErrIdentityMismatch, Identity{Subject: got.Subject, Issuer: got.Issuer}, id)
		}
		return nil, fmt.Errorf("%w: %w", ErrIdentityMismatch, err)
	}
	if err != nil {
		if v.stale != nil {
			return nil, fmt.Errorf("%w; the trust root built into tug was used, which may be older than the signature, because the Sigstore TUF repository could not be reached: %w", err, v.stale)
		}
		return nil, err
	}
	s := &Signer{}
	if res.Signature != nil && res.Signature.Certificate != nil {
		s.Subject = res.Signature.Certificate.SubjectAlternativeName
		s.Issuer = res.Signature.Certificate.Issuer
	}
	for _, ts := range res.VerifiedTimestamps {
		if s.Time.IsZero() || ts.Timestamp.Before(s.Time) {
			s.Time = ts.Timestamp
		}
	}
	return s, nil
}

// signerOf reads the identity in an entity's signing certificate.
func signerOf(entity verify.SignedEntity) (*Signer, error) {
	vc, err := entity.VerificationContent()
	if err != nil {
		return nil, err
	}
	cert := vc.Certificate()
	if cert == nil {
		return nil, errors.New("the bundle has no signing certificate")
	}
	sum, err := certificate.SummarizeCertificate(cert)
	if err != nil {
		return nil, err
	}
	return &Signer{Subject: sum.SubjectAlternativeName, Issuer: sum.Issuer}, nil
}
