package oracle

import (
	"bufio"
	"crypto/md5"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/paingha/tugcli/pkg/provider"
)

// ociConfig is an OCI CLI config file: profiles of key=value entries.
type ociConfig struct {
	path     string
	profiles map[string]map[string]string
	// order lists the profiles as the file does.
	order []string
}

// readConfig reads the config file at path.
func readConfig(path string) (*ociConfig, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, provider.Errorf(provider.Unauthenticated, "there is no OCI config file at %s", path).
			WithHint("run `oci setup config` to create one with an API key, or set " + ConfigFile + " to the file to use")
	}
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "reading the OCI config file: %v", err)
	}
	defer f.Close()
	cfg, err := parseConfig(f)
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "reading %s: %v", path, err)
	}
	cfg.path = path
	return cfg, nil
}

// parseConfig reads the INI format the OCI CLI writes: [PROFILE] headers,
// key=value lines, and comments that start with # or ;.
func parseConfig(r io.Reader) (*ociConfig, error) {
	cfg := &ociConfig{profiles: map[string]map[string]string{}}
	var section map[string]string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			name := strings.TrimSpace(line[1 : len(line)-1])
			if cfg.profiles[name] == nil {
				cfg.profiles[name] = map[string]string{}
				cfg.order = append(cfg.order, name)
			}
			section = cfg.profiles[name]
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == nil {
				continue
			}
			section[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return cfg, sc.Err()
}

// profile returns a profile's entries, with those it leaves out taken from
// DEFAULT as the OCI CLI does.
func (c *ociConfig) profile(name string) (map[string]string, bool) {
	own, ok := c.profiles[name]
	if !ok {
		return nil, false
	}
	merged := map[string]string{}
	for k, v := range c.profiles[DefaultProfile] {
		merged[k] = v
	}
	for k, v := range own {
		merged[k] = v
	}
	return merged, true
}

// apiKey is a profile's API signing key and the identity it belongs to.
type apiKey struct {
	tenancy, user, fingerprint string
	// region is the profile's region, which may be empty.
	region string
	key    *rsa.PrivateKey
}

// keyID is the keyId of the request signature.
func (k *apiKey) keyID() string { return k.tenancy + "/" + k.user + "/" + k.fingerprint }

// passphraseHint says how to give tug a key it can read.
const passphraseHint = "tug cannot unlock an API key with a passphrase; write an unencrypted copy with " +
	"`openssl rsa -in <key file> -out <new key file>`, keep it readable only by you, and point key_file at it, " +
	"or use a profile whose key has no passphrase"

// loadAPIKey reads the API key of profile name in cfg. home expands a
// key_file that starts with ~.
func loadAPIKey(cfg *ociConfig, name, home string) (*apiKey, error) {
	entries, ok := cfg.profile(name)
	if !ok {
		names := slices.Clone(cfg.order)
		return nil, provider.Errorf(provider.InvalidConfig, "%s has no profile %s", cfg.path, name).
			WithHint(fmt.Sprintf("set %s to one of %s", ConfigProfile, strings.Join(names, ", ")))
	}
	if entries["security_token_file"] != "" {
		return nil, provider.Errorf(provider.InvalidConfig, "profile %s in %s signs in with a session token, which tug does not use", name, cfg.path).
			WithHint("use a profile with an API key (user, fingerprint, key_file and tenancy); `oci setup config` creates one")
	}
	for _, k := range []string{"user", "fingerprint", "key_file", "tenancy"} {
		if entries[k] == "" {
			return nil, provider.Errorf(provider.InvalidConfig, "profile %s in %s has no %s", name, cfg.path, k).
				WithHint("an API key profile needs user, fingerprint, key_file and tenancy; `oci setup config` writes them")
		}
	}
	if entries["pass_phrase"] != "" {
		return nil, provider.Errorf(provider.InvalidConfig, "the API key of profile %s has a passphrase", name).WithHint(passphraseHint)
	}
	keyPath := expandPath(entries["key_file"], home, filepath.Dir(cfg.path))
	key, err := readKey(keyPath)
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(entries["fingerprint"])
	if got := fingerprint(&key.PublicKey); got != want {
		return nil, provider.Errorf(provider.InvalidConfig, "the key in %s has the fingerprint %s, but profile %s says %s", keyPath, got, name, want).
			WithHint("point key_file at the private key of the API key with that fingerprint, or correct the fingerprint")
	}
	return &apiKey{
		tenancy:     entries["tenancy"],
		user:        entries["user"],
		fingerprint: want,
		region:      entries["region"],
		key:         key,
	}, nil
}

// expandPath resolves a key_file entry: ~ and %HOMEPATH% stand for the home
// directory, and a relative path is relative to the config file's directory.
func expandPath(path, home, dir string) string {
	for _, prefix := range []string{"~", "%HOMEPATH%", "%USERPROFILE%"} {
		if rest, ok := strings.CutPrefix(path, prefix); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
			path = filepath.Join(home, rest)
			break
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path)
}

// readKey reads an unencrypted RSA private key in PEM, PKCS #1 or PKCS #8,
// with or without the OCI_API_KEY line the console adds after it.
func readKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "reading the API key: %v", err).
			WithHint("check key_file in the OCI config file")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, provider.Errorf(provider.InvalidConfig, "%s holds no PEM private key", path).
			WithHint("key_file must name the private key of an API key, in PEM")
	}
	if block.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") {
		return nil, provider.Errorf(provider.InvalidConfig, "the API key in %s has a passphrase", path).WithHint(passphraseHint)
	}
	var key any
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		err = fmt.Errorf("a %s is not an RSA private key", block.Type)
	}
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "reading the API key in %s: %v", path, err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, provider.Errorf(provider.InvalidConfig, "the API key in %s is not an RSA key", path).
			WithHint("OCI API keys are RSA keys; create one with `oci setup keys`")
	}
	return rsaKey, nil
}

// fingerprint is the fingerprint OCI shows for an API key: the MD5 of the
// public key's DER encoding, as colon-separated hex. It names the key and
// nothing relies on it for security.
func fingerprint(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := md5.Sum(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":")
}
