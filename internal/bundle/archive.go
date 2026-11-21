package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Suffix ends the name of every bundle archive tug names.
const Suffix = ".tar.gz"

// DefaultName names a bundle after its cluster, its target and when it was
// collected: tug-bundle-prod-deployment-api-20261121T153000Z.
func DefaultName(cluster, target string, t time.Time) string {
	parts := []string{"tug-bundle", slug(cluster), slug(target), t.UTC().Format("20060102T150405Z")}
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "-")
}

// NameOf returns the directory name for a bundle written to a file: the
// file's name without .tar.gz or .tgz.
func NameOf(file string) string {
	base := filepath.Base(file)
	for _, ext := range []string{Suffix, ".tgz", ".tar"} {
		if s, ok := strings.CutSuffix(base, ext); ok && s != "" {
			return safe(s)
		}
	}
	return safe(base)
}

// slug turns a cluster or target name into a part of a file name, such as
// an EKS cluster's ARN or "deployment/api".
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-.")
}

// WriteTo writes the bundle as a gzipped tar archive with its files under
// one directory, b.Name, so that unpacking it makes that directory and
// nothing else. It returns how many bytes it wrote.
func (b *Bundle) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	zw := gzip.NewWriter(cw)
	zw.Name = b.Name + ".tar"
	zw.ModTime = b.Created
	tw := tar.NewWriter(zw)
	for _, f := range b.Files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path.Join(b.Name, f.Path),
			Size:     int64(len(f.Data)),
			Mode:     0o600,
			ModTime:  b.Created,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return cw.n, fmt.Errorf("writing %s to the archive: %w", f.Path, err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			return cw.n, fmt.Errorf("writing %s to the archive: %w", f.Path, err)
		}
	}
	if err := tw.Close(); err != nil {
		return cw.n, fmt.Errorf("finishing the archive: %w", err)
	}
	if err := zw.Close(); err != nil {
		return cw.n, fmt.Errorf("finishing the archive: %w", err)
	}
	return cw.n, nil
}

// ErrExists is returned by WriteFile when the file is already there.
var ErrExists = errors.New("file already exists")

// WriteFile writes the bundle's archive to a new file. It refuses to replace
// a file that is there already, and writes to a temporary file beside it
// first, so an interrupted write leaves no half archive under the name.
func (b *Bundle) WriteFile(name string) error {
	switch _, err := os.Lstat(name); {
	case err == nil:
		return fmt.Errorf("%s: %w", name, ErrExists)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	// The archive is the user's to share, not tug's state, so it goes where
	// they asked. It holds what the cluster says about the target (objects,
	// events, logs), and redaction removes only the credentials tug can
	// recognise, so until its owner has reviewed it and chosen whom to give
	// it to, only they may read it: 0o600, which os.CreateTemp also uses.
	f, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*.partial")
	if err != nil {
		return fmt.Errorf("creating %s: %w", name, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // gone once renamed; removes a failed write
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("creating %s: %w", name, err)
	}
	if _, err := b.WriteTo(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := os.Rename(tmp, name); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
