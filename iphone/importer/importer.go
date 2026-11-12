/*
 * Copyright (c) 2026 Gilles Chehade <gilles@plakar.io>
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

// Package importer implements the iphone importer.
//
// The importer backs up an iPhone or iPad connected over USB through Apple's
// backup service, driven by idevicebackup2 from libimobiledevice, and exposes
// the resulting backup directory to plakar.
//
// Location syntax:
//
//	iphone://          the only connected device
//	iphone://<udid>    that device
//
// Config keys (all optional):
//
//	device_id          the device UDID, an alternative to putting it in the location
//	working_directory  where backups are staged; defaults to <user cache dir>/plakar/iphone.
//	                   It persists between runs: that is what makes the next
//	                   backup incremental.
//	full_backup        "true" forces a full backup instead of an incremental one
//	pairing_timeout    how long to wait for the Trust dialog to be accepted (default 60s)
//
// Lifecycle: the constructor finds the tools and the device; Ping makes sure
// the device trusts this host; Import locks the working directory, runs the
// backup, validates it and only then walks it. A backup that did not finish
// is never exposed, so a snapshot either holds a complete backup or does not
// exist.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/poolpOrg/plakar-integrations/iphone/common"
)

// ConfigFullBackup forces a full backup instead of an incremental one.
const ConfigFullBackup = "full_backup"

// FLAG_STREAM matters: without it plakar instantiates a second importer and
// runs Import again to compute a progress summary, which would back up the
// device twice.
const flags = location.FLAG_STREAM

func init() {
	importer.Register("iphone", flags, NewImporter)
}

// Importer is the iphone importer.
type Importer struct {
	tools       *common.Tools
	udid        string
	workdir     string
	full        bool
	pairTimeout time.Duration
	stderr      io.Writer

	mu       sync.Mutex
	paired   bool
	imported bool
	lock     *common.Lock
}

// NewImporter is the constructor registered with plakar.
func NewImporter(ctx context.Context, opts *integration.Options, proto string, config map[string]string) (importer.Importer, error) {
	tools, err := common.LookupTools()
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}
	return New(ctx, opts, proto, config, tools)
}

// New builds an importer over the given tools. It enumerates the connected
// devices and selects the one to back up, so a misconfiguration fails here
// rather than after a snapshot was started.
func New(ctx context.Context, opts *integration.Options, proto string, config map[string]string, tools *common.Tools) (*Importer, error) {
	udid, err := common.ParseLocation(proto, config["location"])
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}
	udid, err = common.ResolveDeviceID(udid, config[common.ConfigDeviceID])
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}

	workdir, err := common.ResolveWorkdir(config[common.ConfigWorkingDirectory])
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}

	full := false
	if v := config[ConfigFullBackup]; v != "" {
		if full, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("iphone: %s: %w", ConfigFullBackup, err)
		}
	}

	pairTimeout, err := common.ParsePairingTimeout(config[common.ConfigPairingTimeout])
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}

	connected, err := tools.ListDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}
	udid, err = common.SelectDevice(connected, udid)
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}

	var stderr io.Writer = os.Stderr
	if opts.Stderr != nil {
		stderr = opts.Stderr
	}

	return &Importer{
		tools:       tools,
		udid:        udid,
		workdir:     workdir,
		full:        full,
		pairTimeout: pairTimeout,
		stderr:      stderr,
	}, nil
}

// UDID is the device this importer backs up.
func (p *Importer) UDID() string { return p.udid }

// Workdir is the directory the backup is staged in.
func (p *Importer) Workdir() string { return p.workdir }

func (p *Importer) Origin() string { return p.udid }
func (p *Importer) Type() string   { return "iphone" }

// Root is "/" rather than "/<udid>": a restore strips the snapshot root
// from the paths it hands the exporter, and the <udid> directory is what
// idevicebackup2 looks for in the restore directory.
func (p *Importer) Root() string                { return "/" }
func (p *Importer) Flags() location.Flags       { return flags }
func (p *Importer) Close(context.Context) error { return p.lock.Release() }

// Ping checks that the device trusts this host, requesting the pairing and
// waiting for the Trust dialog when it does not.
func (p *Importer) Ping(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paired {
		return nil
	}
	if err := p.tools.EnsurePaired(ctx, p.udid, p.pairTimeout, p.stderr); err != nil {
		return fmt.Errorf("iphone: %w", err)
	}
	p.paired = true
	return nil
}

// Import runs the device backup, validates it and streams the backup
// directory. It can only be called once.
func (p *Importer) Import(ctx context.Context, records chan<- *integration.Record, _ <-chan *integration.Result) error {
	defer close(records)

	p.mu.Lock()
	if p.imported {
		p.mu.Unlock()
		return errors.New("iphone: Import called twice on the same importer")
	}
	p.imported = true
	p.mu.Unlock()

	if err := p.Ping(ctx); err != nil {
		return err
	}

	lock, err := common.AcquireLock(common.LockPath(p.workdir, p.udid))
	if err != nil {
		return fmt.Errorf("iphone: %w", err)
	}
	p.lock = lock

	fmt.Fprintf(p.stderr, "iphone: backing up device %s into %s\n", p.udid, p.workdir)
	if err := p.tools.Backup(ctx, p.udid, p.workdir, p.full, p.stderr); err != nil {
		return fmt.Errorf("iphone: backup of device %s failed: %w", p.udid, err)
	}

	info, err := common.ValidateBackup(p.workdir, p.udid)
	if err != nil {
		return fmt.Errorf("iphone: backup of device %s cannot be used: %w", p.udid, err)
	}
	fmt.Fprintf(p.stderr, "iphone: backup of %s finished (%s, %s %s, encrypted: %t)\n",
		p.udid, info.DeviceName, info.ProductType, info.ProductVersion, info.Encrypted)

	return p.walk(ctx, records, info.Dir)
}

// walk streams the backup directory as "/<udid>/..." below a synthetic "/",
// the exact layout idevicebackup2 reads a backup from.
func (p *Importer) walk(ctx context.Context, records chan<- *integration.Record, dir string) error {
	root := "/" + p.udid

	records <- integration.NewRecord("/", "", integration.FileInfo{
		Lname:    "/",
		Lmode:    os.ModeDir | 0700,
		LmodTime: time.Now(),
	}, nil, nil)

	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		pathname := root
		if rel, rerr := filepath.Rel(dir, path); rerr == nil && rel != "." {
			pathname = root + "/" + filepath.ToSlash(rel)
		}

		if err != nil {
			records <- integration.NewError(pathname, err)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			records <- integration.NewError(pathname, err)
			return nil
		}

		fileinfo := integration.FileInfoFromStat(info)
		switch {
		case info.IsDir():
			records <- integration.NewRecord(pathname, "", fileinfo, nil, nil)
		case info.Mode().IsRegular():
			records <- integration.NewRecord(pathname, "", fileinfo, nil, func() (io.ReadCloser, error) {
				return os.Open(path)
			})
		default:
			records <- integration.NewError(pathname, fmt.Errorf("unexpected %s in a device backup", info.Mode().Type()))
		}
		return nil
	})
}
