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

// Package exporter restores files from a snapshot onto the shared storage
// of an Android device over the adb server.  It is a file-level restore:
// it writes files and directories, nothing else.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/android/adb"
	"github.com/poolpOrg/plakar-integrations/android/common"
	"golang.org/x/sync/errgroup"
)

func init() {
	exporter.Register("android", 0, NewExporter)
}

// Exporter writes records under a destination directory on the device.
type Exporter struct {
	opts      *integration.Options
	cfg       *common.Config
	dev       *common.Device
	dest      string
	overwrite bool
	pool      *common.Pool
}

// NewExporter connects to the device and prepares the destination.  With
// no path, files go to a fresh plakar-restore-<timestamp> directory at the
// top of the shared storage, so nothing on the device is ever touched by
// accident.  Writing outside shared storage needs allow_any_path=true.
func NewExporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (exporter.Exporter, error) {
	cfg, err := common.ParseConfig(name, config, "destination_path")
	if err != nil {
		return nil, err
	}

	overwrite := false
	if v, ok := config["overwrite"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("overwrite: %w", err)
		}
		overwrite = b
	}
	anyPath := false
	if v, ok := config["allow_any_path"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("allow_any_path: %w", err)
		}
		anyPath = b
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

	dest := cfg.Path
	if dest == "" {
		root, _, err := common.ResolveRoot(s, "")
		if err != nil {
			return nil, err
		}
		dest = root + "/plakar-restore-" + time.Now().Format("20060102150405")
	}
	if dest == "/" {
		return nil, errors.New("refusing to restore to the root of the device")
	}
	if !anyPath && !common.WithinSharedStorage(dest) {
		return nil, fmt.Errorf("refusing to restore outside shared storage (%s): pass -o allow_any_path=true to override", dest)
	}

	exp := &Exporter{
		opts:      opts,
		cfg:       cfg,
		dev:       dev,
		dest:      dest,
		overwrite: overwrite,
	}

	st, err := s.Lstat(dest)
	switch {
	case err == nil && !st.IsDir():
		if st.IsSymlink() {
			if followed, ferr := s.Stat(dest); ferr == nil && followed.IsDir() {
				break
			}
		}
		return nil, fmt.Errorf("%s exists and is not a directory", dest)
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		if err := exp.mkdirs(ctx, []string{dest}); err != nil {
			return nil, fmt.Errorf("cannot create %s: %w", dest, err)
		}
	default:
		return nil, fmt.Errorf("%s: %w", dest, err)
	}

	return exp, nil
}

func (exp *Exporter) Root() string                    { return exp.dest }
func (exp *Exporter) Origin() string                  { return exp.dev.Serial }
func (exp *Exporter) Type() string                    { return "android" }
func (exp *Exporter) Flags() location.Flags           { return 0 }
func (exp *Exporter) Ping(ctx context.Context) error  { return exp.dev.Ping(ctx) }
func (exp *Exporter) Close(ctx context.Context) error { return nil }

// relative turns a record path into a path under the destination.  The
// record path must be clean and absolute; the result must stay under the
// destination.
func (exp *Exporter) target(pathname string) (string, error) {
	clean := path.Clean("/" + pathname)
	if clean != pathname {
		return "", fmt.Errorf("record path is not clean %q", pathname)
	}
	target := exp.dest
	if clean != "/" {
		target = exp.dest + clean
	}
	if target != exp.dest && !strings.HasPrefix(target, exp.dest+"/") {
		return "", fmt.Errorf("record path %q escapes the destination", pathname)
	}
	if len(target) > adb.MaxPathLength {
		return "", adb.ErrNameTooLong
	}
	return target, nil
}

// Export writes the records.  Files are pushed concurrently through the
// pool; directories are created in batches at the end (pushing a file
// already creates its parents, so only empty directories need it).
func (exp *Exporter) Export(ctx context.Context, _ map[string]string, records <-chan *integration.Record, results chan<- *integration.Result) (ret error) {
	defer close(results)

	exp.pool = common.NewPool(ctx, exp.dev, exp.cfg.Streams)
	defer exp.pool.Close()

	var g errgroup.Group
	g.SetLimit(exp.cfg.Streams)

	var dirs []string

loop:
	for {
		select {
		case <-ctx.Done():
			ret = ctx.Err()
			break loop

		case record, ok := <-records:
			if !ok {
				break loop
			}

			if record.Err != nil || record.IsXattr {
				results <- record.Ok()
				continue
			}

			target, err := exp.target(record.Pathname)
			if err != nil {
				results <- record.Error(err)
				continue
			}

			mode := record.FileInfo.Lmode
			switch {
			case mode.IsDir():
				dirs = append(dirs, target)
				results <- record.Ok()
			case mode.IsRegular():
				g.Go(func() error {
					if err := exp.file(ctx, record, target); err != nil {
						results <- record.Error(err)
					} else {
						results <- record.Ok()
					}
					return nil
				})
			case mode&os.ModeSymlink != 0:
				g.Go(func() error {
					if err := exp.symlink(ctx, record, target); err != nil {
						results <- record.Error(err)
					} else {
						results <- record.Ok()
					}
					return nil
				})
			default:
				results <- record.Error(fmt.Errorf("%s: cannot restore a %s on an Android device", record.Pathname, record.FileInfo.Type()))
			}
		}
	}

	if err := g.Wait(); err != nil && ret == nil {
		ret = err
	}
	if ret != nil {
		return ret
	}
	if err := exp.mkdirs(ctx, dirs); err != nil {
		return fmt.Errorf("creating directories: %w", err)
	}
	return nil
}

// file pushes a regular file.  An existing file is left alone when it has
// the same size and mtime, replaced when overwrite is set, and reported
// as an error otherwise.
func (exp *Exporter) file(ctx context.Context, record *integration.Record, target string) error {
	s, err := exp.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer exp.pool.Release(s)

	fileinfo := record.FileInfo
	if st, err := s.Lstat(target); err == nil {
		switch {
		case st.IsDir():
			return fmt.Errorf("%s: is a directory on the device", target)
		case exp.overwrite:
		case st.IsRegular() && st.Size == fileinfo.Size() && st.ModTime.Unix() == fileinfo.ModTime().Unix():
			// Same size and mtime: taken as already restored.
			return nil
		default:
			return fmt.Errorf("%s: already exists on the device; pass -o overwrite=true to replace it", target)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", target, err)
	}

	perm := uint32(fileinfo.Mode().Perm())
	if perm == 0 {
		perm = 0o644
	}
	if err := s.Send(target, adb.S_IFREG|perm, fileinfo.ModTime(), record.Reader); err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	return nil
}

// symlink recreates a symlink.  Android's shared storage refuses them;
// the device's answer is reported as is.
func (exp *Exporter) symlink(ctx context.Context, record *integration.Record, target string) error {
	s, err := exp.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer exp.pool.Release(s)

	if _, err := s.Lstat(target); err == nil && !exp.overwrite {
		return fmt.Errorf("%s: already exists on the device; pass -o overwrite=true to replace it", target)
	}
	if err := s.Send(target, adb.S_IFLNK|0o777, record.FileInfo.ModTime(), strings.NewReader(record.Target)); err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	return nil
}

// mkdirs creates directories with "mkdir -p", a few dozen per shell
// command.  Every path is quoted.
func (exp *Exporter) mkdirs(ctx context.Context, dirs []string) error {
	const (
		maxBatch = 64
		maxBytes = 16 * 1024
	)
	var errs []error
	for len(dirs) > 0 {
		n, size := 0, 0
		for n < len(dirs) && n < maxBatch && size+len(dirs[n]) < maxBytes {
			size += len(dirs[n]) + 3
			n++
		}
		if n == 0 {
			n = 1
		}
		batch := dirs[:n]
		dirs = dirs[n:]

		args := append([]string{"mkdir", "-p", "--"}, batch...)
		res, err := exp.dev.Client.Shell(ctx, exp.dev.Serial, exp.dev.Features, args...)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			msg := strings.TrimSpace(string(res.Stderr))
			if msg == "" {
				msg = fmt.Sprintf("mkdir exited with status %d", res.ExitCode)
			}
			errs = append(errs, errors.New(msg))
			if len(errs) >= 10 {
				break
			}
		}
	}
	return errors.Join(errs...)
}
