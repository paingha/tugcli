package plugin

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// Revocation is one archive an index has withdrawn, such as a release found
// to steal credentials. tug plugin install refuses it, whichever index or
// file it comes from, and tug plugin ls warns about an installed copy.
type Revocation struct {
	// SHA256 is the archive's sha256, as the index listed it.
	SHA256 string `json:"sha256"`
	// Plugin and Version name what the archive held. They are shown to
	// people, and match plugins installed before tug recorded each
	// install's sha256.
	Plugin  string `json:"plugin,omitempty"`
	Version string `json:"version,omitempty"`
	// Reason says why, in a sentence for the people who installed it.
	Reason string `json:"reason,omitempty"`
}

// String names the revoked archive and says why it was revoked.
func (r Revocation) String() string {
	what := "archive " + r.SHA256
	if r.Plugin != "" {
		what = strings.TrimSpace(r.Plugin + " " + r.Version)
	}
	if r.Reason != "" {
		return what + " (" + r.Reason + ")"
	}
	return what
}

// SupersededKey is a key an index is no longer signed with. When the key
// was replaced rather than retired, Successor is the new key and Signature
// is the old key's signature over RotationMessage(Successor): the old key's
// own word that the new one takes its place. tug moves from a key in
// config.yaml to its successor on that word alone, so a publisher can
// rotate its key without each user editing config.yaml first.
type SupersededKey struct {
	PublicKey string `json:"publicKey"`
	Successor string `json:"successor,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// rotationContext keeps a rotation signature from meaning anything else.
const rotationContext = "tug.plugin.index/v1alpha1 key rotation\n"

// RotationMessage is what an index key signs to hand over to successor.
func RotationMessage(successor ed25519.PublicKey) []byte {
	return append([]byte(rotationContext), successor...)
}

// handsOver reports whether k is a valid handover from key to a successor,
// and returns the successor.
func (k SupersededKey) handsOver(key ed25519.PublicKey) (ed25519.PublicKey, bool) {
	from, err := ParsePublicKey(k.PublicKey)
	if err != nil || !from.Equal(key) || k.Successor == "" {
		return nil, false
	}
	to, err := ParsePublicKey(k.Successor)
	if err != nil {
		return nil, false
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k.Signature))
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, RotationMessage(to), sig) {
		return nil, false
	}
	return to, true
}

func (k SupersededKey) validate() error {
	from, err := ParsePublicKey(k.PublicKey)
	if err != nil {
		return fmt.Errorf("supersededKeys: %w", err)
	}
	switch {
	case k.Successor == "" && k.Signature == "":
		return nil
	case k.Successor == "" || k.Signature == "":
		return fmt.Errorf("supersededKeys entry for %s needs both a successor and the old key's signature, or neither", k.PublicKey)
	}
	if _, ok := k.handsOver(from); !ok {
		return fmt.Errorf("supersededKeys entry for %s: the signature is not that key's over its successor %s", k.PublicKey, k.Successor)
	}
	if k.Successor == k.PublicKey {
		return fmt.Errorf("supersededKeys entry for %s names itself as its successor", k.PublicKey)
	}
	return nil
}

func (r Revocation) validate() error {
	var errs []error
	if _, err := hex.DecodeString(r.SHA256); err != nil || len(r.SHA256) != 64 {
		errs = append(errs, fmt.Errorf("revoked entry %q needs the archive's sha256", r.SHA256))
	}
	if r.Plugin != "" && !validName.MatchString(r.Plugin) {
		errs = append(errs, fmt.Errorf("revoked entry %s: plugin name %q is not valid", r.SHA256, r.Plugin))
	}
	if r.Version != "" && !validPluginVersion.MatchString(r.Version) {
		errs = append(errs, fmt.Errorf("revoked entry %s: %q is not a version", r.SHA256, r.Version))
	}
	return errors.Join(errs...)
}

// RevocationFor returns the revocation of the archive with this sha256, if
// the index lists one.
func (ix *Index) RevocationFor(sha256 string) (Revocation, bool) {
	i := slices.IndexFunc(ix.Revoked, func(r Revocation) bool { return strings.EqualFold(r.SHA256, sha256) })
	if i < 0 {
		return Revocation{}, false
	}
	return ix.Revoked[i], true
}

// RevocationForVersion returns a revocation that names this plugin and
// version, for an installed plugin whose archive's sha256 tug did not
// record.
func (ix *Index) RevocationForVersion(name, version string) (Revocation, bool) {
	i := slices.IndexFunc(ix.Revoked, func(r Revocation) bool {
		return r.Plugin == name && r.Version != "" && sameVersion(r.Version, version)
	})
	if i < 0 {
		return Revocation{}, false
	}
	return ix.Revoked[i], true
}

// SignedBy returns the key whose signature ParseIndex checked: the key it
// was given, or a successor of it.
func (ix *Index) SignedBy() ed25519.PublicKey { return ix.signer }

// successors follows the handovers in the index from key, and returns the
// keys it reaches in order, key first. A handover to a key already reached
// ends the chain.
func (ix *Index) successors(key ed25519.PublicKey) []ed25519.PublicKey {
	chain := []ed25519.PublicKey{key}
	for len(chain) <= len(ix.SupersededKeys) {
		var next ed25519.PublicKey
		for _, k := range ix.SupersededKeys {
			if to, ok := k.handsOver(chain[len(chain)-1]); ok {
				next = to
				break
			}
		}
		if next == nil || slices.ContainsFunc(chain, func(k ed25519.PublicKey) bool { return k.Equal(next) }) {
			break
		}
		chain = append(chain, next)
	}
	return chain
}

// superseded reports whether the index lists key as superseded.
func (ix *Index) superseded(key ed25519.PublicKey) bool {
	return slices.ContainsFunc(ix.SupersededKeys, func(k SupersededKey) bool {
		from, err := ParsePublicKey(k.PublicKey)
		return err == nil && from.Equal(key)
	})
}

// readIndexFile reads an index file to change it, or starts an empty index
// when the file is missing.
func readIndexFile(path string) (*Index, error) {
	ix := &Index{APIVersion: IndexAPIVersion, Kind: IndexKind}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ix, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.UnmarshalStrict(b, ix); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ix, nil
}

// writeIndexFile checks ix and writes it to path. The index is published,
// so it is readable like other release files.
func writeIndexFile(path string, ix *Index) error {
	if err := ix.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(ix)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o644)
}

// RevokeInIndex records r in the index file at path, replacing an earlier
// revocation of the same archive. When r names no plugin, it is taken from
// the index's entry for the archive.
func RevokeInIndex(path string, r Revocation) (Revocation, error) {
	ix, err := readIndexFile(path)
	if err != nil {
		return Revocation{}, err
	}
	r.SHA256 = strings.ToLower(r.SHA256)
	if r.Plugin == "" {
		for _, p := range ix.Plugins {
			for _, v := range p.Versions {
				if strings.EqualFold(v.SHA256, r.SHA256) {
					r.Plugin, r.Version = p.Name, v.Version
				}
			}
		}
	}
	ix.Revoked = slices.DeleteFunc(ix.Revoked, func(x Revocation) bool { return strings.EqualFold(x.SHA256, r.SHA256) })
	ix.Revoked = append(ix.Revoked, r)
	return r, writeIndexFile(path, ix)
}

// RotateIndexKey records in the index file at path that the key whose
// private half is oldPrivate hands over to the one whose private half is
// newPrivate, both base64 as NewIndexKey returns them. The index must then
// be signed with the new key. It returns the two public keys.
func RotateIndexKey(path, oldPrivate, newPrivate string) (oldPublic, newPublic string, err error) {
	oldKey, err := parsePrivateKey(oldPrivate)
	if err != nil {
		return "", "", fmt.Errorf("the old key: %w", err)
	}
	newKey, err := parsePrivateKey(newPrivate)
	if err != nil {
		return "", "", fmt.Errorf("the new key: %w", err)
	}
	oldPub, newPub := oldKey.Public().(ed25519.PublicKey), newKey.Public().(ed25519.PublicKey)
	if oldPub.Equal(newPub) {
		return "", "", errors.New("the old and new keys are the same key")
	}
	ix, err := readIndexFile(path)
	if err != nil {
		return "", "", err
	}
	if ix.superseded(newPub) {
		return "", "", errors.New("the index lists the new key as superseded already; make another with tug plugin index keygen")
	}
	oldPublic, newPublic = base64.StdEncoding.EncodeToString(oldPub), base64.StdEncoding.EncodeToString(newPub)
	ix.SupersededKeys = slices.DeleteFunc(ix.SupersededKeys, func(k SupersededKey) bool { return k.PublicKey == oldPublic })
	ix.SupersededKeys = append(ix.SupersededKeys, SupersededKey{
		PublicKey: oldPublic,
		Successor: newPublic,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(oldKey, RotationMessage(newPub))),
	})
	return oldPublic, newPublic, writeIndexFile(path, ix)
}

// parsePrivateKey reads a base64 ed25519 private key from NewIndexKey.
func parsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("the signing key is not a base64 ed25519 private key")
	}
	return ed25519.PrivateKey(b), nil
}
