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

// Package faketools stands in for the libimobiledevice tools in tests. It
// answers idevice_id, idevicepair and idevicebackup2 invocations the way the
// real tools do (same arguments, exit codes and messages, taken from their
// sources) and writes backup directories shaped like the device would.
package faketools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/poolpOrg/plakar-integrations/iphone/common"
	"howett.net/plist"
)

// Call is one recorded tool invocation.
type Call struct {
	Tool string
	Args []string
	Env  []string
}

// Fake is a scripted stand-in for the tools.
type Fake struct {
	mu sync.Mutex

	// Devices are the UDIDs idevice_id -l lists.
	Devices []string
	// Paired lists the devices that trust the host from the start.
	Paired []string
	// PairDenied makes `idevicepair pair` fail as if the user tapped
	// "Don't Trust".
	PairDenied bool
	// PairAttemptsPending is how many `pair` calls fail with "accept the
	// trust dialog" before one succeeds, as when the user takes a while to
	// tap Trust.
	PairAttemptsPending int

	// BackupExit is the exit status of `idevicebackup2 backup`; the backup
	// directory is only written when it is zero.
	BackupExit int
	// BackupState is the SnapshotState written to Status.plist; empty means
	// "finished".
	BackupState string
	// BackupEncrypted sets IsEncrypted in Manifest.plist.
	BackupEncrypted bool
	// BackupBlocks makes the backup wait for its context to end, as a tool
	// stuck on an unresponsive device would.
	BackupBlocks bool
	// BackupOutput is streamed as the tool's progress output.
	BackupOutput string

	// RestoreExit is the exit status of `idevicebackup2 restore`.
	RestoreExit int

	paired       map[string]bool
	pairAttempts map[string]int
	calls        []Call
}

// New returns a Fake with one connected, paired device.
func New(udids ...string) *Fake {
	return &Fake{Devices: udids, Paired: udids}
}

// Tools returns a common.Tools that runs through the fake, polling the
// pairing fast enough for tests.
func (f *Fake) Tools() *common.Tools {
	tools := common.NewTools(map[string]string{
		common.IdeviceIDTool:     "/fake/bin/" + common.IdeviceIDTool,
		common.IdevicePairTool:   "/fake/bin/" + common.IdevicePairTool,
		common.IdeviceBackupTool: "/fake/bin/" + common.IdeviceBackupTool,
	}, f.Run)
	tools.PairPollInterval = 10 * time.Millisecond
	return tools
}

// Calls returns the recorded invocations of tool, all of them when tool is
// empty.
func (f *Fake) Calls(tool string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.calls {
		if tool == "" || c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func (f *Fake) record(inv *common.Invocation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{
		Tool: filepath.Base(inv.Path),
		Args: slices.Clone(inv.Args),
		Env:  slices.Clone(inv.Env),
	})
}

func (f *Fake) isPaired(udid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paired == nil {
		f.paired = make(map[string]bool)
		for _, u := range f.Paired {
			f.paired[u] = true
		}
	}
	return f.paired[udid]
}

// pair answers a `pair` request: "denied", "pending" or "paired".
func (f *Fake) pair(udid string) string {
	f.isPaired(udid) // initialize
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PairDenied {
		return "denied"
	}
	if f.pairAttempts == nil {
		f.pairAttempts = make(map[string]int)
	}
	if f.pairAttempts[udid] < f.PairAttemptsPending {
		f.pairAttempts[udid]++
		return "pending"
	}
	f.paired[udid] = true
	return "paired"
}

func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// Run is the common.RunFunc of the fake.
func (f *Fake) Run(ctx context.Context, inv *common.Invocation) (*common.Result, error) {
	f.record(inv)
	tool := filepath.Base(inv.Path)
	out := func(code int, text string) (*common.Result, error) {
		if inv.Stream != nil {
			fmt.Fprint(inv.Stream, text)
			return &common.Result{ExitCode: code, Output: []byte(text)}, nil
		}
		return &common.Result{ExitCode: code, Stdout: []byte(text), Output: []byte(text)}, nil
	}

	switch tool {
	case common.IdeviceIDTool:
		if !slices.Equal(inv.Args, []string{"-l"}) {
			return out(2, "unexpected arguments\n")
		}
		var b strings.Builder
		for _, u := range f.Devices {
			b.WriteString(u + "\n")
		}
		return out(0, b.String())

	case common.IdevicePairTool:
		udid := flag(inv.Args, "-u")
		if !slices.Contains(f.Devices, udid) {
			return out(1, fmt.Sprintf("No device found with udid %s.\n", udid))
		}
		switch inv.Args[len(inv.Args)-1] {
		case "validate":
			if f.isPaired(udid) {
				return out(0, fmt.Sprintf("SUCCESS: Validated pairing with device %s\n", udid))
			}
			return out(1, fmt.Sprintf("ERROR: Device %s is not paired with this host\n", udid))
		case "pair":
			switch f.pair(udid) {
			case "denied":
				return out(1, fmt.Sprintf("ERROR: Device %s said that the user denied the trust dialog.\n", udid))
			case "pending":
				return out(1, fmt.Sprintf("ERROR: Please accept the trust dialog on the screen of device %s, then attempt to pair again.\n", udid))
			}
			return out(0, fmt.Sprintf("SUCCESS: Paired with device %s\n", udid))
		}
		return out(2, "ERROR: Invalid command\n")

	case common.IdeviceBackupTool:
		udid := flag(inv.Args, "-u")
		dir := inv.Args[len(inv.Args)-1]
		if _, err := os.Stat(dir); err != nil {
			return out(255, fmt.Sprintf("ERROR: Backup directory \"%s\" does not exist!\n", dir))
		}
		if !slices.Contains(f.Devices, udid) {
			return out(255, fmt.Sprintf("No device found with udid %s.\n", udid))
		}
		if !f.isPaired(udid) {
			return out(255, "ERROR: Could not connect to lockdownd, error code -17\n")
		}
		switch {
		case slices.Contains(inv.Args, "backup"):
			if f.BackupBlocks {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			if f.BackupExit != 0 {
				return out(f.BackupExit, f.BackupOutput+"Backup Failed (Error Code 1).\n")
			}
			full := slices.Contains(inv.Args, "--full")
			if _, err := os.Stat(filepath.Join(dir, udid, common.InfoPlist)); err != nil {
				full = true
			}
			if err := WriteBackup(dir, udid, Layout{State: f.BackupState, Encrypted: f.BackupEncrypted, Full: full}); err != nil {
				return nil, err
			}
			return out(0, f.BackupOutput+"Backup Successful.\n")

		case slices.Contains(inv.Args, "restore"):
			source := flag(inv.Args, "-s")
			if _, err := os.Stat(filepath.Join(dir, source, common.InfoPlist)); err != nil {
				return out(255, fmt.Sprintf("ERROR: Backup directory \"%s\" is invalid. No Info.plist found for UDID %s.\n", dir, source))
			}
			if _, err := common.ValidateBackup(dir, source); err != nil {
				return out(255, "ERROR: Cannot ensure we restore from a successful backup. Aborting.\n")
			}
			if f.RestoreExit != 0 {
				return out(f.RestoreExit, "Restore Failed (Error Code 1).\n")
			}
			return out(0, "Restore Successful.\n")
		}
		return out(2, "ERROR: Unsupported command.\n")
	}
	return nil, fmt.Errorf("faketools: unknown tool %s", tool)
}

// Layout parameterizes a fake backup directory.
type Layout struct {
	// State is the SnapshotState of Status.plist; empty means finished.
	State     string
	Encrypted bool
	Full      bool
	// DeviceName defaults to "Test iPhone".
	DeviceName string
}

// Files is the set of hashed files WriteBackup puts in the backup, with
// their content.
var Files = map[string]string{
	"ab/abcdef0123456789abcdef0123456789abcdef01": "HomeDomain-Library/Preferences/com.apple.test.plist",
	"ab/abcdef0123456789abcdef0123456789abcdef02": "CameraRollDomain-Media/DCIM/100APPLE/IMG_0001.HEIC",
	"ff/ffffffffffffffffffffffffffffffffffffffff": "MediaDomain-Media/Recordings/memo.m4a",
}

// WriteBackup writes dir/<udid> the way idevicebackup2 and the device leave
// it: Info.plist as XML, Manifest.plist and Status.plist as binary plists,
// a Manifest.db and a few hashed files.
func WriteBackup(dir, udid string, l Layout) error {
	if l.State == "" {
		l.State = common.SnapshotFinished
	}
	if l.DeviceName == "" {
		l.DeviceName = "Test iPhone"
	}
	devdir := filepath.Join(dir, udid)
	if err := os.MkdirAll(devdir, 0755); err != nil {
		return err
	}

	now := time.Now().UTC().Truncate(time.Second)
	info := map[string]any{
		"Build Version":     "21F90",
		"Device Name":       l.DeviceName,
		"Display Name":      l.DeviceName,
		"GUID":              "0123456789ABCDEF0123456789ABCDEF",
		"Last Backup Date":  now,
		"Product Type":      "iPhone14,2",
		"Product Version":   "17.5.1",
		"Serial Number":     "F0000000000",
		"Target Identifier": udid,
		"Target Type":       "Device",
		"Unique Identifier": strings.ToUpper(udid),
		"iTunes Version":    "10.0.1",
	}
	manifest := map[string]any{
		"IsEncrypted":          l.Encrypted,
		"Version":              "10.0",
		"Date":                 now,
		"SystemDomainsVersion": "26.0",
		"WasPasscodeSet":       true,
		"Lockdown": map[string]any{
			"BuildVersion":   "21F90",
			"DeviceName":     l.DeviceName,
			"ProductType":    "iPhone14,2",
			"ProductVersion": "17.5.1",
			"UniqueDeviceID": udid,
		},
	}
	status := map[string]any{
		"BackupState":   "new",
		"Date":          now,
		"IsFullBackup":  l.Full,
		"SnapshotState": l.State,
		"UUID":          "6F3A2B1C-0000-4000-8000-000000000001",
		"Version":       "3.3",
	}

	if err := writePlist(filepath.Join(devdir, common.InfoPlist), info, plist.XMLFormat); err != nil {
		return err
	}
	if err := writePlist(filepath.Join(devdir, common.ManifestPlist), manifest, plist.BinaryFormat); err != nil {
		return err
	}
	if err := writePlist(filepath.Join(devdir, common.StatusPlist), status, plist.BinaryFormat); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(devdir, common.ManifestDB), []byte("SQLite format 3\x00fake manifest"), 0644); err != nil {
		return err
	}
	for name, content := range Files {
		p := filepath.Join(devdir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}

func writePlist(path string, v any, format int) error {
	data, err := plist.Marshal(v, format)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
