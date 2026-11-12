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

package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Metadata is the part of backup.json the builder and the restore need.
type Metadata struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Compressed *bool  `json:"compressed"`
	Protected  bool   `json:"protected"`
	Addons     []struct {
		Slug string `json:"slug"`
	} `json:"addons"`
	Folders []string `json:"folders"`
}

// IsCompressed reports whether the inner tars are gzipped.
func (m *Metadata) IsCompressed() bool {
	return m.Compressed == nil || *m.Compressed
}

// AddonSlugs lists the add-ons the backup holds.
func (m *Metadata) AddonSlugs() []string {
	var slugs []string
	for _, a := range m.Addons {
		slugs = append(slugs, a.Slug)
	}
	return slugs
}

type memberWriter struct {
	f  *os.File
	tw *tar.Writer
}

// Builder assembles a backup archive from entries handed over in any
// order, staging the inner tars on disk until WriteTo.
type Builder struct {
	dir  string
	meta []byte
	md   Metadata

	order   []string
	members map[string]*memberWriter
	blobs   map[string]string
}

// NewBuilder creates a builder staging its files in a temporary dir.
func NewBuilder() (*Builder, error) {
	dir, err := os.MkdirTemp("", "plakar-homeassistant-")
	if err != nil {
		return nil, err
	}
	return &Builder{
		dir:     dir,
		members: make(map[string]*memberWriter),
		blobs:   make(map[string]string),
	}, nil
}

// Close removes the staging dir.
func (b *Builder) Close() error {
	for _, m := range b.members {
		m.f.Close()
	}
	return os.RemoveAll(b.dir)
}

// SetMetadata records backup.json.
func (b *Builder) SetMetadata(raw []byte) error {
	var md Metadata
	if err := json.Unmarshal(raw, &md); err != nil {
		return fmt.Errorf("%s: %w", MetadataName, err)
	}
	b.meta = raw
	b.md = md
	return nil
}

// Metadata returns what SetMetadata parsed, nil before it was called.
func (b *Builder) Metadata() *Metadata {
	if b.meta == nil {
		return nil
	}
	return &b.md
}

// AddBlob stages an inner tar kept as is.
func (b *Builder) AddBlob(name string, r io.Reader) error {
	if _, dup := b.blobs[name]; dup {
		return fmt.Errorf("%s: duplicate member", name)
	}
	if _, dup := b.members[name]; dup {
		return fmt.Errorf("%s: both a member directory and a blob", name)
	}

	f, err := os.CreateTemp(b.dir, "blob-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("%s: %w", name, err)
	}
	b.blobs[name] = f.Name()
	b.order = append(b.order, name)
	return nil
}

// AddEntry appends an entry to the inner tar named stem.  r is read
// for regular files only.
func (b *Builder) AddEntry(stem string, hdr *tar.Header, r io.Reader) error {
	m, ok := b.members[stem]
	if !ok {
		f, err := os.CreateTemp(b.dir, "member-*.tar")
		if err != nil {
			return err
		}
		m = &memberWriter{f: f, tw: tar.NewWriter(f)}
		b.members[stem] = m
		b.order = append(b.order, stem)
	}

	if err := m.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("%s/%s: %w", stem, hdr.Name, err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil
	}
	n, err := io.Copy(m.tw, r)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", stem, hdr.Name, err)
	}
	if n != hdr.Size {
		return fmt.Errorf("%s/%s: read %d bytes, expected %d", stem, hdr.Name, n, hdr.Size)
	}
	return nil
}

// WriteTo writes the assembled backup archive.
func (b *Builder) WriteTo(w io.Writer) (int64, error) {
	if b.meta == nil {
		return 0, fmt.Errorf("no %s", MetadataName)
	}

	cw := &countWriter{w: w}
	tw := tar.NewWriter(cw)
	now := time.Now()

	if err := b.writeBytes(tw, MetadataName, b.meta, now); err != nil {
		return cw.n, err
	}

	for _, name := range b.order {
		if p, ok := b.blobs[name]; ok {
			if err := b.writeFile(tw, name, p, now); err != nil {
				return cw.n, err
			}
			continue
		}

		m := b.members[name]
		if err := m.tw.Close(); err != nil {
			return cw.n, fmt.Errorf("%s: %w", name, err)
		}
		if err := m.f.Close(); err != nil {
			return cw.n, fmt.Errorf("%s: %w", name, err)
		}

		src := m.f.Name()
		tarName := name + ".tar"
		if b.md.IsCompressed() {
			gz, err := b.compress(src)
			if err != nil {
				return cw.n, fmt.Errorf("%s: %w", name, err)
			}
			// the plain copy is not needed anymore, keep the staging
			// footprint to one copy of each member
			os.Remove(src)
			src = gz
			tarName += ".gz"
		}
		if err := b.writeFile(tw, tarName, src, now); err != nil {
			return cw.n, err
		}
	}

	if err := tw.Close(); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

func (b *Builder) compress(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	out, err := os.CreateTemp(b.dir, "member-*.tar.gz")
	if err != nil {
		return "", err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if zerr := zw.Close(); err == nil {
		err = zerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

func (b *Builder) writeBytes(tw *tar.Writer, name string, data []byte, when time.Time) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "./" + name,
		Mode:     0644,
		Size:     int64(len(data)),
		ModTime:  when,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (b *Builder) writeFile(tw *tar.Writer, name, src string, when time.Time) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "./" + filepath.ToSlash(name),
		Mode:     0644,
		Size:     st.Size(),
		ModTime:  when,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

var _ io.WriterTo = (*Builder)(nil)

// ErrNoMetadata is returned when a restore set lacks backup.json.
var ErrNoMetadata = errors.New("backup.json is not part of the restore set, restore a whole snapshot")
