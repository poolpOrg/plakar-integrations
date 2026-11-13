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

// Package importer backs up an Immich server the way Immich recommends: a
// fresh database dump taken through the admin API, then, when the server's
// UPLOAD_LOCATION is reachable, a walk of it minus what the server can
// regenerate.  The dump comes first so that the worst case of a backup
// taken while the server runs is files the database does not know about.
package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/PlakarKorp/integration-sdk/location"
	fsimporter "github.com/PlakarKorp/integrations/fs/importer"
	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/dump"
	immich "github.com/poolpOrg/plakar-integrations/immich/importer"
)

const (
	// DefaultDumpTimeout bounds the wait for the dump job.
	DefaultDumpTimeout = 15 * time.Minute

	// pollInterval is how often the backups list is read while waiting.
	pollInterval = 2 * time.Second
)

type Importer struct {
	opts   *connectors.Options
	client *api.Client

	// uploadLocation is the server's UPLOAD_LOCATION on this host, "" when
	// only the dump is backed up.
	uploadLocation string

	timeout  time.Duration
	interval time.Duration
}

func NewImporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (importer.Importer, error) {
	cfg, err := immich.ClientConfig(config)
	if err != nil {
		return nil, err
	}
	client, err := api.New(cfg)
	if err != nil {
		return nil, err
	}

	timeout, err := dump.ParseDuration(config, "dump_timeout", DefaultDumpTimeout)
	if err != nil {
		return nil, err
	}

	uploadLocation := ""
	if loc := config["upload_location"]; loc != "" {
		abs, err := filepath.Abs(loc)
		if err != nil {
			return nil, fmt.Errorf("upload_location %q: %w", loc, err)
		}
		// The fs importer walks the real path; resolve it once so the
		// records come back under one prefix.
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("upload_location %q: %w", loc, err)
		}
		uploadLocation = resolved
	}

	if opts == nil {
		opts = &connectors.Options{MaxConcurrency: 1}
	}
	return &Importer{opts: opts, client: client, uploadLocation: uploadLocation, timeout: timeout, interval: pollInterval}, nil
}

func (i *Importer) Origin() string              { return i.client.Host() }
func (i *Importer) Type() string                { return dump.Scheme }
func (i *Importer) Root() string                { return "/" }
func (i *Importer) Flags() location.Flags       { return location.FLAG_STREAM }
func (i *Importer) Close(context.Context) error { return nil }

// Ping lists the dumps, which takes an admin key.
func (i *Importer) Ping(ctx context.Context) error {
	if _, err := i.client.ListBackups(ctx); err != nil {
		return fmt.Errorf("immich: %w", err)
	}
	return nil
}

func (i *Importer) Import(ctx context.Context, records chan<- *connectors.Record, _ <-chan *connectors.Result) (map[string]string, error) {
	defer close(records)

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
		return nil, err
	}
	if err := send(dirRecord(dump.BackupsDir, now)); err != nil {
		return nil, err
	}

	b, err := Trigger(ctx, i.client, i.timeout, i.interval)
	if err != nil {
		return nil, fmt.Errorf("immich: %w", err)
	}
	modTime := now
	if info, err := dump.Parse(b.Filename, b.Timezone); err == nil {
		modTime = info.Timestamp
	}
	name, size := b.Filename, b.Filesize
	r := connectors.NewRecord(path.Join(dump.BackupsDir, name), "", fileInfo(name, size, modTime), nil,
		func() (io.ReadCloser, error) {
			rc, err := i.client.DownloadBackup(ctx, name, size)
			if err != nil {
				return nil, fmt.Errorf("download %s: %w", name, err)
			}
			return rc, nil
		})
	if err := send(r); err != nil {
		return nil, err
	}

	if i.uploadLocation != "" {
		if err := i.walk(ctx, send); err != nil {
			return nil, fmt.Errorf("immich: %w", err)
		}
	}
	return map[string]string{}, nil
}

// Trigger queues the backup-database job and waits for the dump it writes
// to show up in the backups list.  The list only shows finished dumps: the
// server writes a .tmp file and renames it at the end.
func Trigger(ctx context.Context, c *api.Client, timeout, interval time.Duration) (*api.Backup, error) {
	before, err := c.ListBackups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dumps: %w", err)
	}
	known := make(map[string]struct{}, len(before))
	for _, b := range before {
		known[b.Filename] = struct{}{}
	}

	if err := c.RunJob(ctx, api.JobBackupDatabase); err != nil {
		return nil, fmt.Errorf("queue the %s job: %w", api.JobBackupDatabase, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		backups, err := c.ListBackups(ctx)
		if err != nil {
			return nil, fmt.Errorf("list dumps: %w", err)
		}
		for i := range backups {
			b := &backups[i]
			if _, ok := known[b.Filename]; ok {
				continue
			}
			if strings.HasPrefix(b.Filename, "immich-db-backup-") {
				return b, nil
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the %s job produced no dump within %s; check the server log and the backupDatabase queue", api.JobBackupDatabase, timeout)
		}
	}
}

// walk hands UPLOAD_LOCATION to the fs importer and moves its records
// under /upload_location, leaving out what the server regenerates.
func (i *Importer) walk(ctx context.Context, send func(*connectors.Record) error) error {
	root := toslash(i.uploadLocation)

	opts := *i.opts
	opts.Excludes = slices.Clone(opts.Excludes)
	for _, d := range dump.Regenerable {
		opts.Excludes = append(opts.Excludes, root+"/"+d)
	}

	fs, err := fsimporter.NewFSImporter(ctx, &opts, "fs", map[string]string{"location": "fs://" + i.uploadLocation})
	if err != nil {
		return fmt.Errorf("walk %s: %w", i.uploadLocation, err)
	}
	defer func() { _ = fs.Close(ctx) }()

	inner := make(chan *connectors.Record, max(1, opts.MaxConcurrency))
	walked := make(chan error, 1)
	go func() {
		_, err := fs.Import(ctx, inner, nil)
		walked <- err
	}()

	// Whatever happens, drain inner so the walker's workers can finish.
	var sendErr error
	for r := range inner {
		if sendErr != nil {
			continue
		}
		p, ok := rewrite(root, r.Pathname)
		if !ok {
			continue
		}
		r.Pathname = p
		if p == dump.UploadDir {
			r.FileInfo.Lname = path.Base(dump.UploadDir)
		}
		sendErr = send(r)
	}

	if err := <-walked; err != nil {
		return fmt.Errorf("walk %s: %w", i.uploadLocation, err)
	}
	return sendErr
}

// rewrite moves a path under the walked root to /upload_location and drops
// the rest: the parents of the root, and the regenerable directories.
func rewrite(root, p string) (string, bool) {
	if p == root {
		return dump.UploadDir, true
	}
	if !strings.HasPrefix(p, root+"/") {
		return "", false
	}
	rel := p[len(root):]
	first, _, _ := strings.Cut(rel[1:], "/")
	if slices.Contains(dump.Regenerable, first) {
		return "", false
	}
	return dump.UploadDir + rel, true
}

// toslash is the fs importer's spelling of a local path.
func toslash(p string) string {
	if vol := filepath.VolumeName(p); strings.HasSuffix(vol, ":") {
		p = vol[:len(vol)-1] + p[len(vol):]
	}
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func fileInfo(name string, size int64, modTime time.Time) objects.FileInfo {
	return objects.FileInfo{Lname: name, Lsize: size, Lmode: 0o600, LmodTime: modTime, Lnlink: 1}
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
