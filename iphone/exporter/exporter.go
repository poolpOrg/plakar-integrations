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

// Package exporter implements the iphone exporter.
//
// The exporter materializes a snapshot taken by the iphone importer into a
// backup directory idevicebackup2 can read and, only when asked to, pushes it
// onto a device with `idevicebackup2 restore`.
//
// Location syntax:
//
//	iphone://          restore to the only connected device
//	iphone://<udid>    restore to that device
//
// Config keys (all optional):
//
//	device_id          the target UDID, an alternative to putting it in the location
//	working_directory  where the backup directory is materialized; defaults to
//	                   <user cache dir>/plakar/iphone. Each restore gets its own
//	                   subdirectory under <working_directory>/restore.
//	confirm            "true" actually restores onto the device once the
//	                   directory is complete. Without it the exporter stops after
//	                   writing the directory and prints the command to run. A
//	                   device restore replaces the data on the device and reboots
//	                   it, so it never happens implicitly.
//	backup_password    the password of an encrypted backup, needed to restore it.
//	                   The BACKUP_PASSWORD environment variable is honoured as
//	                   well and keeps the password off the command line.
//	restore_system     "true" restores system files too (--system)
//	restore_settings   "true" restores device settings from the backup (--settings)
//	remove_items       "true" removes items that are not being restored (--remove)
//	no_reboot          "true" does not reboot the device when done (--no-reboot)
//	skip_apps          "true" does not reinstall apps after the restore (--skip-apps)
//	pairing_timeout    how long to wait for the Trust dialog to be accepted (default 60s)
//
// The exporter expects the snapshot to hold exactly one "/<udid>" backup
// directory, as the importer produces. It refuses to restore anything whose
// Status.plist does not say the backup finished, which is the same check
// idevicebackup2 makes.
package exporter

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/iphone/common"
)

// Exporter config keys.
const (
	ConfigConfirm         = "confirm"
	ConfigBackupPassword  = "backup_password"
	ConfigRestoreSystem   = "restore_system"
	ConfigRestoreSettings = "restore_settings"
	ConfigRemoveItems     = "remove_items"
	ConfigNoReboot        = "no_reboot"
	ConfigSkipApps        = "skip_apps"
)

// PasswordEnv is the environment variable idevicebackup2 reads the backup
// password from; the exporter honours it so the password stays off the
// command line.
const PasswordEnv = "BACKUP_PASSWORD"

func init() {
	exporter.Register("iphone", 0, NewExporter)
}

// Exporter is the iphone exporter.
type Exporter struct {
	opts        *integration.Options
	tools       *common.Tools
	udid        string // target device; set only when confirm is on
	workdir     string
	staging     string
	root        *os.Root
	confirm     bool
	restore     common.RestoreOptions
	pairTimeout time.Duration
	stderr      io.Writer

	exported bool
	failures int
}

// NewExporter is the constructor registered with plakar.
func NewExporter(ctx context.Context, opts *integration.Options, proto string, config map[string]string) (exporter.Exporter, error) {
	return New(ctx, opts, proto, config, nil)
}

// New builds an exporter. tools may be nil, in which case they are looked up
// in PATH, and only when confirm is set: writing the directory needs none.
func New(ctx context.Context, opts *integration.Options, proto string, config map[string]string, tools *common.Tools) (*Exporter, error) {
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

	pairTimeout, err := common.ParsePairingTimeout(config[common.ConfigPairingTimeout])
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}

	bools := map[string]*bool{}
	var confirm bool
	restore := common.RestoreOptions{Password: config[ConfigBackupPassword]}
	bools[ConfigConfirm] = &confirm
	bools[ConfigRestoreSystem] = &restore.System
	bools[ConfigRestoreSettings] = &restore.Settings
	bools[ConfigRemoveItems] = &restore.Remove
	bools[ConfigNoReboot] = &restore.NoReboot
	bools[ConfigSkipApps] = &restore.SkipApps
	for key, dst := range bools {
		if v := config[key]; v != "" {
			if *dst, err = strconv.ParseBool(v); err != nil {
				return nil, fmt.Errorf("iphone: %s: %w", key, err)
			}
		}
	}
	if restore.Password == "" {
		restore.Password = os.Getenv(PasswordEnv)
	}

	if confirm {
		if tools == nil {
			if tools, err = common.LookupTools(); err != nil {
				return nil, fmt.Errorf("iphone: %w", err)
			}
		}
		connected, err := tools.ListDevices(ctx)
		if err != nil {
			return nil, fmt.Errorf("iphone: %w", err)
		}
		if udid, err = common.SelectDevice(connected, udid); err != nil {
			return nil, fmt.Errorf("iphone: %w", err)
		}
	}

	restoreDir := filepath.Join(workdir, "restore")
	if err := os.MkdirAll(restoreDir, 0700); err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}
	staging, err := os.MkdirTemp(restoreDir, "restore-*")
	if err != nil {
		return nil, fmt.Errorf("iphone: %w", err)
	}
	root, err := os.OpenRoot(staging)
	if err != nil {
		os.Remove(staging)
		return nil, fmt.Errorf("iphone: %w", err)
	}

	var stderr io.Writer = os.Stderr
	if opts.Stderr != nil {
		stderr = opts.Stderr
	}

	return &Exporter{
		opts:        opts,
		tools:       tools,
		udid:        udid,
		workdir:     workdir,
		staging:     staging,
		root:        root,
		confirm:     confirm,
		restore:     restore,
		pairTimeout: pairTimeout,
		stderr:      stderr,
	}, nil
}

// Staging is the directory the backup is materialized in.
func (p *Exporter) Staging() string { return p.staging }

func (p *Exporter) Root() string               { return p.staging }
func (p *Exporter) Origin() string             { return p.opts.Hostname }
func (p *Exporter) Type() string               { return "iphone" }
func (p *Exporter) Flags() location.Flags      { return 0 }
func (p *Exporter) Ping(context.Context) error { return nil }

// relative turns a record pathname into a path inside the staging root.
func relative(pathname string) (string, error) {
	p := path.Clean("/" + pathname)
	if p != pathname {
		return "", fmt.Errorf("record path is not clean %q", pathname)
	}
	if p == "/" {
		return ".", nil
	}
	rel, err := filepath.Localize(p[1:])
	if err != nil {
		return "", fmt.Errorf("localize %q: %w", pathname, err)
	}
	return rel, nil
}

// Export writes the records into the staging directory and, once the
// stream ends, finishes the restore: validation, then the device restore
// when confirm is set. Finishing here rather than in Close makes its outcome
// the outcome of the restore command.
func (p *Exporter) Export(ctx context.Context, records <-chan *integration.Record, results chan<- *integration.Result) error {
	defer close(results)
	p.exported = true

	for {
		var record *integration.Record
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case record, ok = <-records:
			if !ok {
				return p.finish(ctx)
			}
		}

		if record.Err != nil || record.IsXattr {
			results <- record.Ok()
			continue
		}

		if err := p.write(record); err != nil {
			p.failures++
			results <- record.Error(err)
			continue
		}
		results <- record.Ok()
	}
}

func (p *Exporter) write(record *integration.Record) error {
	rel, err := relative(record.Pathname)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}

	mode := record.FileInfo.Mode()
	switch {
	case mode.IsDir():
		if err := p.root.Mkdir(rel, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		return nil
	case mode.IsRegular():
		return p.writeFile(record, rel)
	default:
		return fmt.Errorf("unexpected %s in a device backup", mode.Type())
	}
}

// writeFile writes the record through a temporary file in the same directory
// and renames it into place, so an interrupted restore never leaves a
// truncated file under its final name.
func (p *Exporter) writeFile(record *integration.Record, rel string) error {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(rel), ".plakar-"+base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]))

	f, err := p.root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			p.root.Remove(tmp)
		}
	}()

	if _, err := io.Copy(f, record.Reader); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := p.root.Rename(tmp, rel); err != nil {
		return err
	}
	ok = true

	mtime := record.FileInfo.ModTime()
	if mtime.IsZero() {
		return nil
	}
	return p.root.Chtimes(rel, mtime, mtime)
}

// Close releases the staging root. An exporter that never exported leaves
// nothing behind. Closing twice is harmless.
func (p *Exporter) Close(context.Context) error {
	err := p.closeRoot()
	if !p.exported {
		os.Remove(p.staging)
	}
	return err
}

// closeRoot closes the staging root once. The directory handle has to be
// closed before the directory can be removed on Windows.
func (p *Exporter) closeRoot() error {
	if p.root == nil {
		return nil
	}
	root := p.root
	p.root = nil
	return root.Close()
}

// relocate handles a snapshot restored without its "/<udid>" directory,
// which is what plakar hands over for "<snapshot>:/<udid>": the manifests
// land at the top of the staging directory. The device the backup belongs
// to is in Info.plist, so the files are moved under it.
func (p *Exporter) relocate() error {
	if _, err := os.Stat(filepath.Join(p.staging, common.InfoPlist)); err != nil {
		return nil
	}
	udid, err := common.BackupUDID(p.staging)
	if err != nil {
		return err
	}
	if err := p.root.Mkdir(udid, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(p.staging)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == udid {
			continue
		}
		if err := p.root.Rename(e.Name(), filepath.Join(udid, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// finish validates the materialized backup and, when confirm is set,
// restores it onto the device. Without confirm it reports where the
// directory is and how to restore it by hand.
func (p *Exporter) finish(ctx context.Context) error {
	if p.failures > 0 {
		return fmt.Errorf("iphone: %d records failed to restore into %s, device restore not attempted", p.failures, p.staging)
	}
	if err := p.relocate(); err != nil {
		return fmt.Errorf("iphone: %w", err)
	}

	udids, err := common.FindBackups(p.staging)
	if err != nil {
		return fmt.Errorf("iphone: %w", err)
	}
	if len(udids) != 1 {
		return fmt.Errorf("iphone: expected exactly one device backup in the snapshot, found %d in %s", len(udids), p.staging)
	}
	info, err := common.ValidateBackup(p.staging, udids[0])
	if err != nil {
		return fmt.Errorf("iphone: restored backup in %s cannot be restored to a device: %w", p.staging, err)
	}

	fmt.Fprintf(p.stderr, "iphone: backup of %s (%s, %s %s, taken %s, encrypted: %t) written to %s\n",
		info.UDID, info.DeviceName, info.ProductType, info.ProductVersion, info.Date.Format(time.RFC3339), info.Encrypted, p.staging)

	if !p.confirm {
		target := p.udid
		if target == "" {
			target = "<target-udid>"
		}
		fmt.Fprintf(p.stderr, "iphone: nothing was sent to a device. To restore it, run:\n")
		fmt.Fprintf(p.stderr, "    %s -u %s -s %s restore %s\n", common.IdeviceBackupTool, target, info.UDID, p.staging)
		fmt.Fprintf(p.stderr, "or restore again with -o %s=true\n", ConfigConfirm)
		return nil
	}

	if info.Encrypted && p.restore.Password == "" {
		return fmt.Errorf("iphone: backup of %s is encrypted, set %s or %s to restore it (directory kept at %s)",
			info.UDID, ConfigBackupPassword, PasswordEnv, p.staging)
	}

	if err := p.tools.EnsurePaired(ctx, p.udid, p.pairTimeout, p.stderr); err != nil {
		return fmt.Errorf("iphone: %w", err)
	}

	// Serialize with backups of the same device: both mutate the device.
	lock, err := common.AcquireLock(common.LockPath(p.workdir, p.udid))
	if err != nil {
		return fmt.Errorf("iphone: %w", err)
	}
	defer lock.Release()

	p.restore.TargetUDID = p.udid
	p.restore.SourceUDID = info.UDID
	p.restore.Dir = p.staging

	fmt.Fprintf(p.stderr, "iphone: restoring backup of %s onto device %s\n", info.UDID, p.udid)
	if err := p.tools.Restore(ctx, p.restore, p.stderr); err != nil {
		return fmt.Errorf("iphone: restore onto device %s failed (directory kept at %s): %w", p.udid, p.staging, err)
	}
	fmt.Fprintf(p.stderr, "iphone: restore onto device %s finished\n", p.udid)

	if err := p.closeRoot(); err != nil {
		return fmt.Errorf("iphone: close %s: %w", p.staging, err)
	}
	if err := os.RemoveAll(p.staging); err != nil {
		return fmt.Errorf("iphone: remove %s: %w", p.staging, err)
	}
	return nil
}
