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

package importer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/layout"
)

// Emit sends the folded state down the records channel: directories
// first, the manifest right after the root, then every asset with its side
// files, then albums, stacks and people.  Originals and sidecars are
// fetched when kloset reads them.
func Emit(ctx context.Context, c *api.Client, st *State, records chan<- *connectors.Record) error {
	now := time.Now().UTC()
	send := func(r *connectors.Record) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case records <- r:
			return nil
		}
	}

	if err := send(dirRecord("/", now)); err != nil {
		return err
	}
	manifest, err := jsonRecord(layout.ManifestPath, st.Manifest, now)
	if err != nil {
		return err
	}
	if err := send(manifest); err != nil {
		return err
	}

	// Every directory under which something lands, parents first.
	dirs := make(map[string]struct{})
	addDirs := func(p string) {
		for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
			dirs[d] = struct{}{}
		}
	}
	for _, e := range st.Assets {
		addDirs(e.Path)
	}
	for _, p := range st.AlbumPaths {
		addDirs(p)
	}
	for _, p := range st.StackPaths {
		addDirs(p)
	}
	for _, p := range st.PersonPaths {
		addDirs(p)
	}
	sorted := make([]string, 0, len(dirs))
	for d := range dirs {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted) // a parent sorts before its children
	for _, d := range sorted {
		if err := send(dirRecord(d, now)); err != nil {
			return err
		}
	}

	for _, e := range st.Assets {
		if err := emitAsset(ctx, c, e, now, send); err != nil {
			return err
		}
	}
	for i, al := range st.Albums {
		r, err := jsonRecord(st.AlbumPaths[i], al, now)
		if err != nil {
			return err
		}
		if err := send(r); err != nil {
			return err
		}
	}
	for i, s := range st.Stacks {
		r, err := jsonRecord(st.StackPaths[i], s, now)
		if err != nil {
			return err
		}
		if err := send(r); err != nil {
			return err
		}
	}
	for i, p := range st.People {
		r, err := jsonRecord(st.PersonPaths[i], p, now)
		if err != nil {
			return err
		}
		if err := send(r); err != nil {
			return err
		}
	}
	return nil
}

func emitAsset(ctx context.Context, c *api.Client, e *Entry, now time.Time, send func(*connectors.Record) error) error {
	meta, err := jsonRecord(e.Path+".json", e.Asset, now)
	if err != nil {
		return err
	}
	if err := send(meta); err != nil {
		return err
	}

	id := e.Asset.ImmichID
	size := e.Size
	name := e.Asset.OriginalFileName
	want, err := hex.DecodeString(e.Hex)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	original := connectors.NewRecord(e.Path, "", fileInfo(path.Base(e.Path), size, e.ModTime), nil,
		func() (io.ReadCloser, error) {
			bound := size
			if bound < 0 {
				bound = 0
			}
			rc, err := c.Original(ctx, id, bound)
			if err != nil {
				return nil, fmt.Errorf("download %s: %w", name, err)
			}
			return &verifyReader{rc: rc, h: sha1.New(), want: want, name: name}, nil
		})
	if err := send(original); err != nil {
		return err
	}

	if e.SidecarFileID != "" {
		fileID := e.SidecarFileID
		sidecar := connectors.NewRecord(e.Path+".xmp", "", fileInfo(path.Base(e.Path)+".xmp", -1, e.ModTime), nil,
			func() (io.ReadCloser, error) {
				rc, err := c.AssetFileDownload(ctx, fileID)
				if err != nil {
					return nil, fmt.Errorf("download sidecar of %s: %w", name, err)
				}
				return rc, nil
			})
		if err := send(sidecar); err != nil {
			return err
		}
	}
	return nil
}

func fileInfo(name string, size int64, modTime time.Time) objects.FileInfo {
	return objects.FileInfo{Lname: name, Lsize: size, Lmode: 0o644, LmodTime: modTime, Lnlink: 1}
}

func dirRecord(p string, now time.Time) *connectors.Record {
	name := path.Base(p)
	if p == "/" {
		name = "/"
	}
	fi := objects.FileInfo{Lname: name, Lmode: os.ModeDir | 0o755, LmodTime: now, Lnlink: 1}
	return connectors.NewRecord(p, "", fi, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	})
}

func jsonRecord(p string, v any, now time.Time) (*connectors.Record, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", p, err)
	}
	b = append(b, '\n')
	return connectors.NewRecord(p, "", fileInfo(path.Base(p), int64(len(b)), now), nil,
		func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(b)), nil
		}), nil
}

// verifyReader hashes what passes through and turns the EOF into an error
// when the bytes are not the ones Immich announced.
type verifyReader struct {
	rc   io.ReadCloser
	h    hash.Hash
	want []byte
	name string
}

func (v *verifyReader) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
	}
	if err == io.EOF {
		if got := v.h.Sum(nil); !bytes.Equal(got, v.want) {
			return n, fmt.Errorf("%s: checksum mismatch, got %x want %x", v.name, got, v.want)
		}
	}
	return n, err
}

func (v *verifyReader) Close() error { return v.rc.Close() }
