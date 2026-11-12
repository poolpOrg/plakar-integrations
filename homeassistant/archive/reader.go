/*
 * Copyright (c) 2026 Gilles Chehade <gilles@poolp.org>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

// Package archive reads and writes Home Assistant backup archives.
//
// A backup is a plain tar holding backup.json and one inner tar per
// component: homeassistant.tar.gz for the configuration directory, one
// per add-on and one per shared folder on supervised installs.  Inner
// tars are gzipped when backup.json says compressed, and encrypted when
// the backup is protected.
//
// Open explodes the inner tars it can read so that each file inside
// them becomes an entry of its own, which is what makes deduplication
// across backups work.  Inner tars it cannot read, encrypted ones in
// practice, are kept as opaque blobs.  Builder does the reverse.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MetadataName is the file describing the backup inside the archive.
const MetadataName = "backup.json"

// maxMetadata bounds backup.json, which is a few hundred bytes.
const maxMetadata = 16 << 20

var errNotTar = errors.New("not a tar archive")

// Entry is one file, directory or symlink inside a member.
type Entry struct {
	Name   string // cleaned path inside the member, "data/configuration.yaml"
	Header *tar.Header

	file *os.File
	off  int64
	size int64
}

// Open returns the content of a regular entry.
func (e *Entry) Open() (io.ReadCloser, error) {
	if e.file == nil {
		return nil, fmt.Errorf("%s: not a regular file", e.Name)
	}
	return io.NopCloser(io.NewSectionReader(e.file, e.off, e.size)), nil
}

// Member is one inner tar of the backup.
type Member struct {
	Name    string   // name in the outer tar, "homeassistant.tar.gz"
	Stem    string   // Name without the tar suffix, set when exploded
	Entries []*Entry // the exploded inner tar, nil for a blob
	Blob    *Entry   // the inner tar kept as is, nil when exploded
}

// Archive is an opened backup.
type Archive struct {
	Metadata []byte // backup.json
	Members  []*Member

	outerPath string
	outer     *os.File
	inner     *os.File // decompressed inner tars, appended one after the other
	innerSize int64
}

// Open indexes the backup tar at pathname.  The archive owns the file
// from then on and removes it on Close.
func Open(pathname string) (*Archive, error) {
	f, err := os.Open(pathname)
	if err != nil {
		return nil, err
	}

	a := &Archive{outerPath: pathname, outer: f}
	if err := a.load(); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// Close releases the spool files.
func (a *Archive) Close() error {
	var errs []error
	if a.outer != nil {
		errs = append(errs, a.outer.Close(), os.Remove(a.outerPath))
		a.outer = nil
	}
	if a.inner != nil {
		errs = append(errs, a.inner.Close(), os.Remove(a.inner.Name()))
		a.inner = nil
	}
	return errors.Join(errs...)
}

func (a *Archive) load() error {
	entries, err := index(a.outer, a.outer, 0)
	if err != nil {
		return fmt.Errorf("reading the backup archive: %w", err)
	}

	for _, e := range entries {
		if strings.Contains(e.Name, "/") {
			return fmt.Errorf("unexpected nested entry %q in the backup archive", e.Name)
		}
		if e.Header.Typeflag == tar.TypeDir {
			continue
		}
		if e.file == nil {
			return fmt.Errorf("unexpected entry %q in the backup archive", e.Name)
		}

		if e.Name == MetadataName {
			if e.size > maxMetadata {
				return fmt.Errorf("%s is too large (%d bytes)", MetadataName, e.size)
			}
			rd, err := e.Open()
			if err != nil {
				return err
			}
			a.Metadata, err = io.ReadAll(rd)
			if err != nil {
				return fmt.Errorf("reading %s: %w", MetadataName, err)
			}
			continue
		}

		m, err := a.explode(e)
		if err != nil {
			return fmt.Errorf("reading %s: %w", e.Name, err)
		}
		a.Members = append(a.Members, m)
	}

	if a.Metadata == nil {
		return fmt.Errorf("no %s in the backup archive", MetadataName)
	}
	return nil
}

// splitTarName returns the stem of an inner tar name and whether it is
// gzipped: "core_mosquitto.tar.gz" is ("core_mosquitto", true, true).
func splitTarName(name string) (stem string, gz, ok bool) {
	if s, found := strings.CutSuffix(name, ".tar.gz"); found && s != "" {
		return s, true, true
	}
	if s, found := strings.CutSuffix(name, ".tar"); found && s != "" {
		return s, false, true
	}
	return "", false, false
}

// explode indexes an inner tar, decompressing it into the inner spool
// first when gzipped.  Anything that does not read as a tar, an
// encrypted member in practice, is kept as a blob.
func (a *Archive) explode(e *Entry) (*Member, error) {
	m := &Member{Name: e.Name, Blob: e}

	stem, gz, ok := splitTarName(e.Name)
	if !ok {
		return m, nil
	}

	var (
		entries []*Entry
		err     error
	)
	if gz {
		entries, err = a.explodeGzip(e)
	} else {
		entries, err = index(io.NewSectionReader(e.file, e.off, e.size), e.file, e.off)
	}
	if errors.Is(err, errNotTar) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}

	m.Stem = stem
	m.Entries = entries
	m.Blob = nil
	return m, nil
}

func (a *Archive) explodeGzip(e *Entry) ([]*Entry, error) {
	zr, err := gzip.NewReader(io.NewSectionReader(e.file, e.off, e.size))
	if err != nil {
		return nil, errNotTar
	}
	defer zr.Close()

	if a.inner == nil {
		f, err := os.CreateTemp(filepath.Dir(a.outerPath), "plakar-homeassistant-*.spool")
		if err != nil {
			return nil, err
		}
		a.inner = f
	}

	start := a.innerSize
	n, err := io.Copy(a.inner, zr)
	a.innerSize += n
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}

	return index(io.NewSectionReader(a.inner, start, n), a.inner, start)
}

// counter tracks how far a tar reader got into its source.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// cleanName normalizes a tar entry name; it returns "" for the archive
// root and an error for names escaping it.
func cleanName(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("refusing entry name %q", name)
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("refusing entry name %q", name)
	}
	return cleaned, nil
}

// index lists the entries of the tar read from r, whose first byte sits
// at base in file, so that regular files can be read back at random.
func index(r io.Reader, file *os.File, base int64) ([]*Entry, error) {
	cr := &counter{r: r}
	tr := tar.NewReader(cr)

	var entries []*Entry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			if len(entries) == 0 {
				return nil, errNotTar
			}
			return nil, err
		}

		name, err := cleanName(hdr.Name)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}

		e := &Entry{Name: name, Header: hdr}
		switch hdr.Typeflag {
		case tar.TypeReg:
			e.file = file
			e.off = base + cr.n
			e.size = hdr.Size
		case tar.TypeDir, tar.TypeSymlink:
		case tar.TypeLink:
			return nil, fmt.Errorf("hard link %q is not supported", hdr.Name)
		default:
			// devices, fifos and sockets: nothing to keep
			continue
		}
		entries = append(entries, e)
	}
}
