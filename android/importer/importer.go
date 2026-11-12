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

// Package importer backs up the shared storage of an Android device over
// the adb server, streaming file contents straight into kloset.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/exclude"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/android/adb"
	"github.com/poolpOrg/plakar-integrations/android/common"
)

func init() {
	importer.Register("android", 0, NewImporter)
}

// Importer walks a device directory and emits one record per entry.
type Importer struct {
	opts     *integration.Options
	cfg      *common.Config
	dev      *common.Device
	root     string
	rootStat adb.Stat
	excludes *exclude.RuleSet
	pool     *common.Pool
}

// NewImporter connects to the device named by the configuration and
// checks that the requested path (or the shared storage) is accessible.
func NewImporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (importer.Importer, error) {
	cfg, err := common.ParseConfig(name, config, "source_path")
	if err != nil {
		return nil, err
	}

	excludes := exclude.NewRuleSet()
	if err := excludes.AddRulesFromArray(opts.Excludes); err != nil {
		return nil, fmt.Errorf("exclude rules: %w", err)
	}

	dev, err := common.Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}

	s, err := dev.Client.Sync(ctx, dev.Serial, dev.Features)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	root, st, err := common.ResolveRoot(s, cfg.Path)
	if err != nil {
		return nil, err
	}

	return &Importer{
		opts:     opts,
		cfg:      cfg,
		dev:      dev,
		root:     root,
		rootStat: st,
		excludes: excludes,
	}, nil
}

func (imp *Importer) Origin() string                 { return imp.dev.Serial }
func (imp *Importer) Type() string                   { return "android" }
func (imp *Importer) Root() string                   { return imp.root }
func (imp *Importer) Flags() location.Flags          { return 0 }
func (imp *Importer) Ping(ctx context.Context) error { return imp.dev.Ping(ctx) }

// Close releases the sync sessions.  Readers handed to kloset may outlive
// Import, so the pool lives until here.
func (imp *Importer) Close(ctx context.Context) error {
	if imp.pool != nil {
		imp.pool.Close()
	}
	return nil
}

// Import enumerates the tree under the root.  Listing uses one session;
// file contents are read lazily by kloset through the pool.  A transport
// failure (the device went away) aborts the import with an error, so an
// interrupted run never passes for a complete snapshot; per-entry failures
// are reported as error records and the walk goes on.
func (imp *Importer) Import(ctx context.Context, records chan<- *integration.Record, _ <-chan *integration.Result) (map[string]string, error) {
	defer close(records)

	if imp.pool != nil {
		imp.pool.Close()
	}
	imp.pool = common.NewPool(ctx, imp.dev, imp.cfg.Streams)

	lister, err := imp.dev.Client.Sync(ctx, imp.dev.Serial, imp.dev.Features)
	if err != nil {
		return nil, err
	}
	defer lister.Close()

	w := &walker{imp: imp, ctx: ctx, lister: lister, records: records}

	// The root itself: a directory to walk, or a single file.
	root := imp.root
	if imp.rootStat.IsRegular() {
		w.emit(w.record(root, path.Base(root), imp.rootStat, ""))
		return imp.metadata(), ctx.Err()
	}
	rootStat := imp.rootStat
	rootStat.Mode = adb.S_IFDIR | rootStat.Mode&0o7777
	if !w.emit(w.record(root, path.Base(root), rootStat, "")) {
		return nil, ctx.Err()
	}
	if err := w.walk(root); err != nil {
		return nil, err
	}
	return imp.metadata(), nil
}

// metadata describes the device the snapshot was taken from.
func (imp *Importer) metadata() map[string]string {
	m := map[string]string{"serial": imp.dev.Serial}
	if imp.dev.Info.Model != "" {
		m["model"] = imp.dev.Info.Model
	}
	if imp.dev.Info.Product != "" {
		m["product"] = imp.dev.Info.Product
	}
	if imp.dev.Info.Name != "" {
		m["device"] = imp.dev.Info.Name
	}
	return m
}

type walker struct {
	imp     *Importer
	ctx     context.Context
	lister  *adb.Sync
	records chan<- *integration.Record
}

// emit sends a record, giving up on cancellation.
func (w *walker) emit(r *integration.Record) bool {
	select {
	case w.records <- r:
		return true
	case <-w.ctx.Done():
		return false
	}
}

func (w *walker) fail(p string, err error) bool {
	return w.emit(integration.NewError(p, err))
}

// walk lists dir, emits its entries and recurses into subdirectories.
// Files are emitted while the listing streams in; only the names of the
// subdirectories are kept, so memory grows with the depth of the tree and
// the fan-out of one directory, never with the whole inventory.
func (w *walker) walk(dir string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}

	var subdirs []string
	readable, err := w.lister.List(dir, func(d adb.Dirent) error {
		if w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		p := dir + "/" + d.Name
		if dir == "/" {
			p = "/" + d.Name
		}
		if d.Err != nil {
			w.fail(p, d.Err)
			return nil
		}
		if len(p) > adb.MaxPathLength {
			w.fail(p, adb.ErrNameTooLong)
			return nil
		}
		isDir := d.Stat.IsDir()
		if w.imp.excludes.IsExcluded(p, isDir) {
			return nil
		}
		switch {
		case isDir:
			if !w.emit(w.record(p, d.Name, d.Stat, "")) {
				return w.ctx.Err()
			}
			subdirs = append(subdirs, p)
		case d.Stat.IsSymlink():
			target, err := w.readlink(p)
			if err != nil {
				w.fail(p, err)
				return nil
			}
			if !w.emit(w.record(p, d.Name, d.Stat, target)) {
				return w.ctx.Err()
			}
		default:
			if !w.emit(w.record(p, d.Name, d.Stat, "")) {
				return w.ctx.Err()
			}
		}
		return nil
	})
	if err != nil {
		if w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		if w.lister.Broken() {
			// The listing stream died: the device is gone or the
			// protocol is off.  Nothing below can be trusted.
			return fmt.Errorf("listing %s: %w", dir, err)
		}
		w.fail(dir, err)
		return nil
	}
	if !readable {
		w.fail(dir, fmt.Errorf("cannot list directory: %w", fs.ErrPermission))
		return nil
	}

	for _, sub := range subdirs {
		if err := w.walk(sub); err != nil {
			return err
		}
	}
	return nil
}

// readlink asks the device shell for a symlink target.  The path is
// quoted; the output is taken as an opaque string.
func (w *walker) readlink(p string) (string, error) {
	res, err := w.imp.dev.Client.Shell(w.ctx, w.imp.dev.Serial, w.imp.dev.Features, "readlink", "--", p)
	if err != nil {
		return "", fmt.Errorf("readlink: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("readlink: %s", strings.TrimSpace(string(res.Stderr)))
	}
	target := strings.TrimSuffix(string(res.Stdout), "\n")
	if target == "" || strings.ContainsAny(target, "\n\x00") {
		return "", fmt.Errorf("readlink: unexpected output")
	}
	return target, nil
}

// record builds the kloset record for an entry.  Regular files get a lazy
// reader that streams the content through the pool when kloset asks.
func (w *walker) record(p, name string, st adb.Stat, target string) *integration.Record {
	fileinfo := objects.FileInfo{
		Lname:    name,
		Lsize:    st.Size,
		Lmode:    st.FileMode(),
		LmodTime: st.ModTime,
		Ldev:     st.Dev,
		Lino:     st.Ino,
		Luid:     uint64(st.UID),
		Lgid:     uint64(st.GID),
		Lnlink:   uint16(min(st.Nlink, 0xFFFF)),
	}
	if !st.IsRegular() {
		fileinfo.Lsize = 0
		if st.IsSymlink() {
			fileinfo.Lsize = int64(len(target))
		}
		return integration.NewRecord(p, target, fileinfo, nil, nil)
	}
	if w.lister.V1Metadata() {
		// v1 sizes are 32-bit: let kloset measure the content instead.
		fileinfo.Lsize = -1
	}
	expected := fileinfo.Lsize
	return integration.NewRecord(p, "", fileinfo, nil, func() (io.ReadCloser, error) {
		return w.imp.open(w.ctx, p, expected)
	})
}

// open starts streaming a file.  The session is returned to the pool when
// the reader is closed.
func (imp *Importer) open(ctx context.Context, p string, expected int64) (io.ReadCloser, error) {
	s, err := imp.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.Recv(p)
	if err != nil {
		imp.pool.Release(s)
		return nil, err
	}
	return &fileReader{r: r, s: s, pool: imp.pool, path: p, expected: expected}, nil
}

type fileReader struct {
	r        *adb.RecvReader
	s        *adb.Sync
	pool     *common.Pool
	path     string
	expected int64
	done     bool
}

func (f *fileReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		if f.expected >= 0 && f.r.Bytes() != f.expected {
			// The file changed while being read: the metadata in the
			// record no longer describes the content.
			return n, fmt.Errorf("%s: size changed during backup (expected %d bytes, read %d)", f.path, f.expected, f.r.Bytes())
		}
		return n, io.EOF
	}
	return n, fmt.Errorf("%s: %w", f.path, err)
}

func (f *fileReader) Close() error {
	if f.done {
		return nil
	}
	f.done = true
	err := f.r.Close()
	f.pool.Release(f.s)
	return err
}
