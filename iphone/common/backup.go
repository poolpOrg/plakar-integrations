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

package common

import (
	"context"
	"io"
)

// Backup runs `idevicebackup2 -u <udid> backup [--full] <dir>`. The tool
// writes the device's backup under dir/<udid>; when that directory already
// holds a backup the device only sends what changed. The caller validates the
// result with [ValidateBackup]: a zero exit alone does not mean the backup is
// complete.
func (t *Tools) Backup(ctx context.Context, udid, dir string, full bool, stream io.Writer) error {
	args := []string{"-u", udid, "backup"}
	if full {
		args = append(args, "--full")
	}
	args = append(args, dir)

	res, err := t.invoke(ctx, IdeviceBackupTool, args, nil, stream)
	if err != nil {
		return err
	}
	return check(IdeviceBackupTool, args, res)
}

// RestoreOptions parameterize a device restore.
type RestoreOptions struct {
	// TargetUDID is the device to restore to.
	TargetUDID string
	// SourceUDID is the subdirectory of Dir holding the backup. It is the
	// UDID of the device the backup was taken from.
	SourceUDID string
	// Dir is the backup root, the directory passed to Backup.
	Dir string
	// Password unlocks an encrypted backup. It is handed to the tool through
	// its environment, never on the command line.
	Password string

	System   bool // --system: restore system files too
	Settings bool // --settings: restore device settings from the backup
	Remove   bool // --remove: remove items which are not being restored
	NoReboot bool // --no-reboot: do not reboot the device when done
	SkipApps bool // --skip-apps: do not reinstall apps after the restore
}

// Args returns the idevicebackup2 arguments for the restore. The password is
// not among them.
func (o RestoreOptions) Args() []string {
	args := []string{"-u", o.TargetUDID, "-s", o.SourceUDID, "restore"}
	if o.System {
		args = append(args, "--system")
	}
	if o.Settings {
		args = append(args, "--settings")
	}
	if o.Remove {
		args = append(args, "--remove")
	}
	if o.NoReboot {
		args = append(args, "--no-reboot")
	}
	if o.SkipApps {
		args = append(args, "--skip-apps")
	}
	return append(args, o.Dir)
}

// Restore runs `idevicebackup2 restore`, pushing the backup under
// Dir/<SourceUDID> onto the target device. The tool itself refuses a backup
// whose Status.plist is not "finished".
func (t *Tools) Restore(ctx context.Context, o RestoreOptions, stream io.Writer) error {
	args := o.Args()
	var env []string
	if o.Password != "" {
		env = []string{"BACKUP_PASSWORD=" + o.Password}
	}

	res, err := t.invoke(ctx, IdeviceBackupTool, args, env, stream)
	if err != nil {
		return err
	}
	return check(IdeviceBackupTool, args, res)
}
