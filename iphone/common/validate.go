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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"howett.net/plist"
)

// Files idevicebackup2 and the device leave in dir/<udid>.
const (
	InfoPlist     = "Info.plist"     // written by idevicebackup2 from lockdown values
	ManifestPlist = "Manifest.plist" // written by the device: encryption, versions
	StatusPlist   = "Status.plist"   // written by the device: snapshot state
	ManifestDB    = "Manifest.db"    // written by the device: file index (iOS 10 and later)
	ManifestMBDB  = "Manifest.mbdb"  // file index of iOS 9 and earlier
)

// SnapshotFinished is the Status.plist SnapshotState of a complete backup; it
// is the value idevicebackup2 itself requires before it restores.
const SnapshotFinished = "finished"

// maxPlistSize bounds how much of a property list is read. Info.plist carries
// app icons and iTunes metadata and reaches a few megabytes on a loaded
// device; anything beyond this is not a backup manifest.
const maxPlistSize = 64 << 20

// IncompleteBackupError is a backup directory whose Status.plist does not say
// the backup finished: the previous run was interrupted or failed.
type IncompleteBackupError struct {
	Dir   string
	State string
}

func (e *IncompleteBackupError) Error() string {
	return fmt.Sprintf("backup in %s is incomplete (snapshot state %q, want %q)", e.Dir, e.State, SnapshotFinished)
}

// BackupInfo describes a validated backup directory.
type BackupInfo struct {
	// UDID of the device the backup was taken from.
	UDID string
	// Dir is the per-device directory, root/<udid>.
	Dir string

	SnapshotState string
	Encrypted     bool
	Version       string
	Date          time.Time
	IsFullBackup  bool

	DeviceName     string
	ProductType    string
	ProductVersion string
	BuildVersion   string
}

type statusPlist struct {
	SnapshotState string    `plist:"SnapshotState"`
	Date          time.Time `plist:"Date"`
	IsFullBackup  bool      `plist:"IsFullBackup"`
	Version       string    `plist:"Version"`
}

type manifestPlist struct {
	IsEncrypted bool      `plist:"IsEncrypted"`
	Version     string    `plist:"Version"`
	Date        time.Time `plist:"Date"`
	Lockdown    struct {
		DeviceName     string `plist:"DeviceName"`
		ProductType    string `plist:"ProductType"`
		ProductVersion string `plist:"ProductVersion"`
		BuildVersion   string `plist:"BuildVersion"`
		UniqueDeviceID string `plist:"UniqueDeviceID"`
	} `plist:"Lockdown"`
}

type infoPlist struct {
	TargetIdentifier string `plist:"Target Identifier"`
	DeviceName       string `plist:"Device Name"`
	ProductType      string `plist:"Product Type"`
	ProductVersion   string `plist:"Product Version"`
	BuildVersion     string `plist:"Build Version"`
}

// ValidateBackup checks that root/<udid> holds a complete backup of udid and
// returns what the manifests say about it. It is the gate between "the tool
// exited" and "this can go into a snapshot": a backup that fails here is never
// exposed.
func ValidateBackup(root, udid string) (*BackupInfo, error) {
	if err := ValidateUDID(udid); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, udid)

	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("backup directory %s is not a directory", dir)
	}

	for _, name := range []string{InfoPlist, ManifestPlist, StatusPlist} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("backup in %s is missing %s: %w", dir, name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestDB)); err != nil {
		if _, err2 := os.Stat(filepath.Join(dir, ManifestMBDB)); err2 != nil {
			return nil, fmt.Errorf("backup in %s is missing %s: %w", dir, ManifestDB, err)
		}
	}

	var status statusPlist
	if err := readPlist(filepath.Join(dir, StatusPlist), &status); err != nil {
		return nil, fmt.Errorf("%s: %w", StatusPlist, err)
	}
	if status.SnapshotState != SnapshotFinished {
		return nil, &IncompleteBackupError{Dir: dir, State: status.SnapshotState}
	}

	var manifest manifestPlist
	if err := readPlist(filepath.Join(dir, ManifestPlist), &manifest); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestPlist, err)
	}

	var info infoPlist
	if err := readPlist(filepath.Join(dir, InfoPlist), &info); err != nil {
		return nil, fmt.Errorf("%s: %w", InfoPlist, err)
	}
	if info.TargetIdentifier != "" && !strings.EqualFold(info.TargetIdentifier, udid) {
		return nil, fmt.Errorf("backup in %s belongs to device %s, not %s", dir, info.TargetIdentifier, udid)
	}

	bi := &BackupInfo{
		UDID:           udid,
		Dir:            dir,
		SnapshotState:  status.SnapshotState,
		Encrypted:      manifest.IsEncrypted,
		Version:        manifest.Version,
		Date:           manifest.Date,
		IsFullBackup:   status.IsFullBackup,
		DeviceName:     info.DeviceName,
		ProductType:    info.ProductType,
		ProductVersion: info.ProductVersion,
		BuildVersion:   info.BuildVersion,
	}
	if bi.DeviceName == "" {
		bi.DeviceName = manifest.Lockdown.DeviceName
	}
	if bi.ProductType == "" {
		bi.ProductType = manifest.Lockdown.ProductType
	}
	if bi.ProductVersion == "" {
		bi.ProductVersion = manifest.Lockdown.ProductVersion
	}
	if bi.BuildVersion == "" {
		bi.BuildVersion = manifest.Lockdown.BuildVersion
	}
	return bi, nil
}

// BackupUDID returns the device a backup directory belongs to, from the
// "Target Identifier" its Info.plist carries. dir is the directory holding
// Info.plist itself.
func BackupUDID(dir string) (string, error) {
	var info infoPlist
	if err := readPlist(filepath.Join(dir, InfoPlist), &info); err != nil {
		return "", fmt.Errorf("%s: %w", InfoPlist, err)
	}
	if err := ValidateUDID(info.TargetIdentifier); err != nil {
		return "", fmt.Errorf("%s: Target Identifier: %w", InfoPlist, err)
	}
	return info.TargetIdentifier, nil
}

// FindBackups returns the UDIDs of the backups present under root, in
// directory order. A backup is a directory named after a UDID that holds an
// Info.plist; nothing else is inspected.
func FindBackups(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var udids []string
	for _, e := range entries {
		if !e.IsDir() || ValidateUDID(e.Name()) != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), InfoPlist)); err != nil {
			continue
		}
		udids = append(udids, e.Name())
	}
	return udids, nil
}

// readPlist decodes an XML or binary property list into v.
func readPlist(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxPlistSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxPlistSize {
		return errors.New("property list is larger than the limit")
	}
	if _, err := plist.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
