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

// Package exporter restores an immich+dump:// snapshot: the dump goes
// through the server's maintenance restore, the files of UPLOAD_LOCATION
// go through the fs exporter, and the server regenerates its thumbnails.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/PlakarKorp/integration-sdk/location"
	fsexporter "github.com/PlakarKorp/integrations/fs/exporter"
	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/dump"
	immich "github.com/poolpOrg/plakar-integrations/immich/importer"
)

const (
	// DefaultRestoreTimeout bounds the wait for the maintenance restore.
	DefaultRestoreTimeout = time.Hour

	// pollInterval is how often the maintenance status is read.
	pollInterval = 2 * time.Second
)

type Exporter struct {
	opts   *connectors.Options
	client *api.Client
	log    io.Writer

	token    string
	force    bool
	timeout  time.Duration
	interval time.Duration

	// fs restores /upload_location into the configured directory; nil
	// when none is configured.
	fs exporter.Exporter

	// restored is the dump applied in this run, "" until one is.
	restored string
}

func NewExporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (exporter.Exporter, error) {
	cfg, err := immich.ClientConfig(config)
	if err != nil {
		return nil, err
	}
	client, err := api.New(cfg)
	if err != nil {
		return nil, err
	}

	token := os.Getenv(dump.EnvMaintenanceToken)
	if token == "" {
		token = config["maintenance_token"]
	}
	force, err := dump.ParseBool(config, "force")
	if err != nil {
		return nil, err
	}
	timeout, err := dump.ParseDuration(config, "restore_timeout", DefaultRestoreTimeout)
	if err != nil {
		return nil, err
	}

	if opts == nil {
		opts = &connectors.Options{MaxConcurrency: 1}
	}

	var fs exporter.Exporter
	if loc := config["upload_location"]; loc != "" {
		abs, err := filepath.Abs(loc)
		if err != nil {
			return nil, fmt.Errorf("upload_location %q: %w", loc, err)
		}
		fs, err = fsexporter.NewFSExporter(ctx, opts, "fs", map[string]string{"location": "fs://" + abs})
		if err != nil {
			return nil, fmt.Errorf("upload_location %q: %w", loc, err)
		}
	}

	return &Exporter{
		opts:     opts,
		client:   client,
		log:      os.Stderr,
		token:    token,
		force:    force,
		timeout:  timeout,
		interval: pollInterval,
		fs:       fs,
	}, nil
}

func (e *Exporter) Origin() string        { return e.client.Host() }
func (e *Exporter) Type() string          { return dump.Scheme }
func (e *Exporter) Root() string          { return "/" }
func (e *Exporter) Flags() location.Flags { return 0 }

// Ping reads the maintenance status, which both the server and its
// maintenance worker answer without credentials.
func (e *Exporter) Ping(ctx context.Context) error {
	if _, err := e.client.MaintenanceStatus(ctx); err != nil {
		return fmt.Errorf("immich: %w", err)
	}
	return nil
}

func (e *Exporter) Close(ctx context.Context) error {
	if e.fs != nil {
		return e.fs.Close(ctx)
	}
	return nil
}

func (e *Exporter) Export(ctx context.Context, meta map[string]string, records <-chan *connectors.Record, results chan<- *connectors.Result) (ret error) {
	defer close(results)

	var files *bridge
	defer func() {
		if files == nil {
			return
		}
		if err := files.finish(); err != nil && ret == nil {
			ret = fmt.Errorf("immich: restore files: %w", err)
		}
		if ret == nil && files.written.Load() > 0 {
			if err := e.client.JobCommand(ctx, api.QueueThumbnailGeneration, "start", true); err != nil {
				ret = fmt.Errorf("immich: start thumbnail generation: %w", err)
			}
		}
	}()

loop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case record, ok := <-records:
			if !ok {
				break loop
			}

			p := path.Clean("/" + record.Pathname)
			switch {
			case record.Err != nil:
				results <- record.Ok()

			case p == dump.UploadDir || strings.HasPrefix(p, dump.UploadDir+"/"):
				if e.fs == nil {
					results <- record.Error(errors.New("no upload_location configured"))
					continue
				}
				if files == nil {
					files = newBridge(ctx, e.fs, meta, results, max(1, e.opts.MaxConcurrency))
				}
				if err := files.send(ctx, record); err != nil {
					return err
				}

			case record.IsXattr, p == "/", p == dump.BackupsDir:
				results <- record.Ok()

			// A restore of snap:/backups alone strips the prefix and
			// hands the dump at the root.
			case strings.HasPrefix(p, dump.BackupsDir+"/") || (path.Dir(p) == "/" && dump.IsDump(p)):
				if record.FileInfo.Lmode.IsDir() {
					results <- record.Ok()
					continue
				}
				if err := e.restore(ctx, path.Base(p), record.Reader); err != nil {
					results <- record.Error(err)
				} else {
					results <- record.Ok()
				}

			default:
				results <- record.Error(errors.New("not part of an immich+dump snapshot"))
			}
		}
	}
	return nil
}

// restore pushes one dump through the server's maintenance restore and
// waits for the server to come back.
func (e *Exporter) restore(ctx context.Context, name string, r io.Reader) error {
	if e.restored != "" {
		return fmt.Errorf("%s was already restored in this run; restore one dump at a time", e.restored)
	}
	if !dump.IsDump(name) {
		return errors.New("not a database dump")
	}

	info, err := dump.Parse(name, "")
	if err != nil && !e.force {
		return fmt.Errorf("%w; pass force=true to restore it without a version check", err)
	}

	st, err := e.client.MaintenanceStatus(ctx)
	if err != nil {
		return fmt.Errorf("maintenance status: %w", err)
	}
	inWorker := st.Active
	if inWorker {
		if e.token == "" {
			return fmt.Errorf("the server is in maintenance mode (%s): pass the maintenance token printed in its log in %s", st.Action, dump.EnvMaintenanceToken)
		}
		if err := e.client.MaintenanceLogin(ctx, e.token); err != nil {
			return fmt.Errorf("maintenance login: %w", err)
		}
	} else {
		cfg, err := e.client.ServerConfig(ctx)
		if err != nil {
			return fmt.Errorf("server config: %w", err)
		}
		if !cfg.IsInitialized {
			return fmt.Errorf("the server has no admin user yet and is not in maintenance mode: "+
				"start the restore on it (POST /api/admin/database-backups/start-restore, or \"Restore from backup\" on its setup page), "+
				"take the maintenance token from its log and pass it in %s", dump.EnvMaintenanceToken)
		}
	}

	if err == nil {
		if err := e.checkVersions(ctx, info, inWorker); err != nil {
			if !e.force {
				return fmt.Errorf("%w; pass force=true to restore it anyway", err)
			}
			logf(e.log, "immich: %v, restoring anyway (force)\n", err)
		}
	}

	logf(e.log, "immich: uploading %s\n", name)
	if err := e.client.UploadBackup(ctx, name, r); err != nil {
		return err
	}
	stored := dump.UploadedPrefix + name

	logf(e.log, "immich: restoring %s\n", stored)
	if err := e.client.SetMaintenance(ctx, api.ActionRestoreDatabase, stored); err != nil {
		// The server restarts into its maintenance worker right away
		// and may cut the connection before answering; only a refusal
		// is final.
		if !transient(err) {
			return fmt.Errorf("start the restore of %s: %w", stored, err)
		}
	}

	if err := WaitRestore(ctx, e.client.MaintenanceStatus, e.interval, e.timeout, e.log); err != nil {
		return fmt.Errorf("restore of %s: %w", stored, err)
	}
	e.restored = name
	logf(e.log, "immich: %s restored\n", stored)
	return nil
}

// checkVersions compares the dump with the target.  The Immich version
// comes from /server/about, or from /server/version where the key cannot be
// used yet; the Postgres version only from the dumps the server made
// itself, since nothing else exposes it.
func (e *Exporter) checkVersions(ctx context.Context, info dump.Info, inWorker bool) error {
	target := ""
	if !inWorker {
		if about, err := e.client.ServerAbout(ctx); err == nil {
			target = about.Version
		}
	}
	if target == "" {
		v, err := e.client.ServerVersion(ctx)
		if err != nil {
			return fmt.Errorf("server version: %w", err)
		}
		target = fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}

	backups, err := e.client.ListBackups(ctx)
	if err != nil {
		return fmt.Errorf("list the server's dumps: %w", err)
	}
	var postgres []string
	for _, b := range backups {
		if strings.HasPrefix(b.Filename, dump.UploadedPrefix) {
			continue
		}
		if bi, err := dump.Parse(b.Filename, ""); err == nil {
			postgres = append(postgres, bi.Postgres)
		}
	}
	if len(postgres) == 0 {
		logf(e.log, "immich: the server has no dump of its own, its Postgres version cannot be checked against pg%s\n", info.Postgres)
	}
	return dump.CheckVersions(info, target, postgres)
}
