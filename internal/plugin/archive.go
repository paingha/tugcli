package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// Limits for archives, so a hostile one cannot fill the disk.
const (
	maxArchiveBytes = 512 << 20
	maxFileBytes    = 256 << 20
	maxFiles        = 1000
)

// PackageResult describes an archive written by Package.
type PackageResult struct {
	Path     string
	SHA256   string
	Manifest *Manifest
	// Missing lists platforms dropped because their binary was not built.
	Missing []Platform
	// Mismatched lists platforms dropped because their binary was built for
	// another OS or architecture.
	Mismatched []Platform
}

// Package writes a plugin directory as an installable .tar.gz in outDir. The
// archived manifest records the sha256 of each platform binary. Platforms
// whose binary is missing, or was built for another OS or architecture, are
// left out. The source manifest is not changed.
func Package(dir, outDir string) (*PackageResult, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	res := &PackageResult{}
	var platforms []Platform
	for _, p := range m.Spec.Platforms {
		bin := filepath.Join(dir, filepath.FromSlash(p.Bin))
		sum, err := fileSHA256(bin)
		if errors.Is(err, os.ErrNotExist) {
			res.Missing = append(res.Missing, p)
			continue
		}
		if err != nil {
			return nil, err
		}
		if !matchesPlatform(bin, p) {
			res.Mismatched = append(res.Mismatched, p)
			continue
		}
		p.SHA256 = sum
		platforms = append(platforms, p)
	}
	if len(platforms) == 0 {
		return nil, fmt.Errorf("no platform binaries are built in %s; build them first", dir)
	}
	m.Spec.Platforms = platforms
	manifest, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := addTarFile(tw, ManifestFile, manifest, 0o644); err != nil {
		return nil, err
	}
	added := map[string]bool{}
	for _, p := range platforms {
		if added[p.Bin] {
			continue
		}
		added[p.Bin] = true
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p.Bin)))
		if err != nil {
			return nil, err
		}
		mode := int64(0o755)
		if p.IsWASM() {
			mode = 0o644 // tug reads a module; nothing executes the file
		}
		if err := addTarFile(tw, p.Bin, data, mode); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}

	name := fmt.Sprintf("%s%s_%s.tar.gz", BinaryPrefix, m.Metadata.Name, m.Metadata.Version)
	res.Path = filepath.Join(outDir, name)
	if err := config.WriteFileAtomic(res.Path, buf.Bytes(), 0o644); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(buf.Bytes())
	res.SHA256 = hex.EncodeToString(sum[:])
	res.Manifest = m
	return res, nil
}

func addTarFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Fetch downloads an archive over HTTPS into dir and returns its path. Plain
// HTTP is accepted only for loopback hosts, for local testing.
func Fetch(ctx context.Context, rawURL, dir string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return "", fmt.Errorf("refusing %s: plugins are downloaded over https only", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s", rawURL, resp.Status)
	}
	f, err := os.CreateTemp(dir, "download-*"+archiveExt(u.Path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("%s is larger than %d MiB", rawURL, maxArchiveBytes>>20)
	}
	return f.Name(), nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func archiveExt(p string) string {
	switch {
	case strings.HasSuffix(p, ".tar.gz"):
		return ".tar.gz"
	case strings.HasSuffix(p, ".tgz"):
		return ".tgz"
	case strings.HasSuffix(p, ".zip"):
		return ".zip"
	default:
		return ""
	}
}

// Staged is an archive extracted into the plugins directory, not yet live.
type Staged struct {
	Plugin *Plugin
	// Receipt is recorded beside the plugin when it is committed. Stage
	// fills in the archive's sha256; the caller adds where it came from.
	Receipt Receipt
	dir     string
}

// Receipt records where an installed plugin came from, so that tug can
// tell later whether its index has revoked it, and who signed it.
type Receipt struct {
	// SHA256 is the sha256 of the archive it was installed from.
	SHA256 string `json:"sha256"`
	// Source is the file or URL of the archive.
	Source string `json:"source,omitempty"`
	// Index is the plugin index it was installed from by name.
	Index string `json:"index,omitempty"`
	// Signer is the Sigstore identity whose signature on the archive tug
	// checked.
	Signer string `json:"signer,omitempty"`
	// InstalledAt is when it was installed.
	InstalledAt time.Time `json:"installedAt,omitzero"`
}

// receiptSuffix names a receipt, which sits beside the version directory
// it describes rather than in it, where the archive's own files are.
const receiptSuffix = ".receipt.yaml"

func receiptPath(versionDir string) string { return versionDir + receiptSuffix }

// Receipt returns the record of how p was installed. Plugins installed
// before tug kept receipts, and linked ones, have none.
func (d Dirs) Receipt(p *Plugin) (Receipt, bool) {
	var r Receipt
	if p.Dev {
		return r, false
	}
	b, err := os.ReadFile(receiptPath(p.Dir))
	if err != nil || yaml.Unmarshal(b, &r) != nil || r.SHA256 == "" {
		return Receipt{}, false
	}
	return r, true
}

// Discard removes a staged plugin.
func (s *Staged) Discard() { os.RemoveAll(s.dir) }

// Stage verifies an archive against wantSHA256 (when given), extracts it
// safely and loads it, checking every binary against the manifest's sha256.
func (d Dirs) Stage(archive, wantSHA256 string) (*Staged, error) {
	if err := os.MkdirAll(d.Installed, 0o700); err != nil {
		return nil, err
	}
	u, err := d.unpack(archive, wantSHA256, d.Installed, ".staging-")
	if err != nil {
		return nil, err
	}
	s := &Staged{Plugin: u.Plugin, dir: u.dir, Receipt: Receipt{SHA256: u.SHA256, Source: archive}}
	if err := errors.Join(u.RunErr, u.Plugin.Manifest.CheckTugVersion(d.TugVersion)); err != nil {
		s.Discard()
		return nil, err
	}
	return s, nil
}

// Unpacked is an archive extracted into a temporary directory to be looked
// at, not installed.
type Unpacked struct {
	// Plugin is the plugin the archive holds. When RunErr is set it has
	// only its Manifest and Dir.
	Plugin *Plugin
	// SHA256 is the archive's sha256.
	SHA256 string
	// RunErr says why the plugin cannot run here: it has no build for
	// this machine.
	RunErr error
	dir    string
}

// Close removes the extracted files.
func (u *Unpacked) Close() error { return os.RemoveAll(u.dir) }

// Unpack extracts an archive into a temporary directory and loads it, as
// Stage does, checking every binary against the manifest's sha256 but not
// whether this tug is new enough to run it. A plugin with no build for this
// machine is unpacked with RunErr set.
func (d Dirs) Unpack(archive string) (*Unpacked, error) {
	return d.unpack(archive, "", "", "tug-plugin-")
}

func (d Dirs) unpack(archive, wantSHA256, parent, pattern string) (*Unpacked, error) {
	sum, err := fileSHA256(archive)
	if err != nil {
		return nil, err
	}
	if wantSHA256 != "" && !strings.EqualFold(sum, wantSHA256) {
		return nil, fmt.Errorf("checksum mismatch: %s is %s, expected %s", filepath.Base(archive), sum, wantSHA256)
	}
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return nil, err
	}
	u := &Unpacked{SHA256: strings.ToLower(sum), dir: dir}
	if err := extract(archive, dir); err != nil {
		_ = u.Close()
		return nil, err
	}
	root, err := pluginRoot(dir)
	if err != nil {
		_ = u.Close()
		return nil, err
	}
	m, err := LoadManifest(root)
	if err != nil {
		_ = u.Close()
		return nil, err
	}
	if len(m.Runnable(d.PreferWASM)) == 0 {
		u.Plugin, u.RunErr = &Plugin{Manifest: m, Dir: root}, m.noBuild()
		return u, nil
	}
	if u.Plugin, err = d.load(root, false); err != nil {
		_ = u.Close()
		return nil, err
	}
	return u, nil
}

// Commit moves a staged plugin to <plugins>/<name>/<version>. An existing
// install of the same version is replaced only when force is set.
func (d Dirs) Commit(s *Staged, force bool) (*Plugin, error) {
	defer s.Discard()
	m := s.Plugin.Manifest
	dest := filepath.Join(d.Installed, m.Metadata.Name, m.Metadata.Version)
	if _, err := os.Stat(dest); err == nil {
		if !force {
			return nil, fmt.Errorf("%s %s is already installed; use --force to replace it", m.Metadata.Name, m.Metadata.Version)
		}
		if err := os.RemoveAll(dest); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return nil, err
	}
	if err := os.Rename(s.Plugin.Dir, dest); err != nil {
		return nil, err
	}
	rc := s.Receipt
	if rc.InstalledAt.IsZero() {
		rc.InstalledAt = time.Now().UTC()
	}
	b, err := yaml.Marshal(rc)
	if err == nil {
		err = config.WriteFileAtomic(receiptPath(dest), b, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("recording how %s %s was installed: %w", m.Metadata.Name, m.Metadata.Version, err)
	}
	return d.load(dest, false)
}

// Remove deletes every installed version of a plugin and returns them.
func (d Dirs) Remove(name string) ([]string, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid plugin name %q", name)
	}
	dir := filepath.Join(d.Installed, name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("plugin %q is not installed", name)
	}
	if err != nil {
		return nil, err
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	return versions, os.RemoveAll(dir)
}

// pluginRoot finds plugin.yaml at the top of an extracted archive, or inside
// its single top-level directory.
func pluginRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		sub := filepath.Join(dir, entries[0].Name())
		if _, err := os.Stat(filepath.Join(sub, ManifestFile)); err == nil {
			return sub, nil
		}
	}
	return "", fmt.Errorf("the archive has no %s at its top level", ManifestFile)
}

// extract unpacks a .tar.gz or .zip into dest. Only regular files and
// directories are allowed, every path must stay inside dest, and sizes are
// capped.
func extract(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return fmt.Errorf("%s is not an archive", filepath.Base(archive))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		return extractTarGz(f, dest)
	case string(magic) == "PK\x03\x04":
		st, err := f.Stat()
		if err != nil {
			return err
		}
		return extractZip(f, st.Size(), dest)
	default:
		return fmt.Errorf("%s is neither a .tar.gz nor a .zip", filepath.Base(archive))
	}
}

// safePath joins an archive member name onto dest, refusing anything that
// would escape it.
func safePath(dest, name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, `\`, "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) || filepath.IsAbs(clean) || strings.Contains(clean, ":") {
		return "", fmt.Errorf("archive member %q escapes the plugin directory", name)
	}
	return filepath.Join(dest, filepath.FromSlash(clean)), nil
}

func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for files := 0; ; files++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if files >= maxFiles {
			return fmt.Errorf("archive has more than %d entries", maxFiles)
		}
		target, err := safePath(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if hdr.Size > maxFileBytes || total > maxArchiveBytes {
				return fmt.Errorf("archive member %s is too large", hdr.Name)
			}
			if err := writeMember(target, tr, hdr.Size, hdr.FileInfo().Mode()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive member %s is a link or special file; only files and directories are allowed", hdr.Name)
		}
	}
}

func extractZip(r io.ReaderAt, size int64, dest string) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}
	if len(zr.File) > maxFiles {
		return fmt.Errorf("archive has more than %d entries", maxFiles)
	}
	var total int64
	for _, zf := range zr.File {
		target, err := safePath(dest, zf.Name)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode.IsRegular():
			n := int64(zf.UncompressedSize64)
			total += n
			if n > maxFileBytes || total > maxArchiveBytes {
				return fmt.Errorf("archive member %s is too large", zf.Name)
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			err = writeMember(target, rc, n, mode)
			rc.Close()
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive member %s is a link or special file; only files and directories are allowed", zf.Name)
		}
	}
	return nil
}

func writeMember(target string, r io.Reader, size int64, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("archive member %s is not the size its header says", filepath.Base(target))
	}
	return nil
}
