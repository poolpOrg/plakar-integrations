# Android (USB) Integration

## Overview

The **Android** integration lets Plakar back up and restore the files of an
Android phone or tablet plugged in over USB, through the Android Debug
Bridge (adb). It needs no root and no application on the device: only USB
debugging, the same thing every Android developer turns on.

This integration allows:

- Backing up the device's shared storage (photos, videos, screenshots,
  downloads, documents, recordings, exported chats...) into a Kloset
  repository, streamed straight from the device, with nothing staged on disk
- Incremental snapshots: files whose metadata did not change since the last
  snapshot are not transferred again
- File-level restore of a snapshot, or part of it, onto the device
- Several devices on the same computer, each addressed by its serial number

It is a **file-level backup of the data the device exposes over adb**, not
a full device backup: private application data (`/data/data`), the system
partitions, SMS, call logs, accounts and settings are not accessible without
root and are not backed up. See *What is backed up*.

## Requirements

- The `adb` tool from the Android SDK platform-tools, installed on the
  computer running plakar (Linux, macOS, Windows). Plakar talks to the adb
  *server* over TCP; it starts it with `adb start-server` when it is not
  running, so `adb` must be in `PATH`, in `$ANDROID_HOME/platform-tools`, or
  pointed at by `PLAKAR_ADB`.
- USB debugging enabled on the device: *Settings > About phone*, tap
  *Build number* seven times, then *Settings > System > Developer options >
  USB debugging*.
- The computer authorized on the device: the first connection pops up
  *Allow USB debugging?* on the phone; tick *Always allow from this computer*
  and accept.
- The device unlocked for the first connection. Shared storage of a device
  that has never been unlocked since boot is not readable.

Check with `adb devices`: the device must show as `device`, not
`unauthorized` or `offline`.

## Configuration

Both connectors take the location `android://[serial][/path]`.

| Option | Importer | Exporter | Description |
|---|---|---|---|
| `location` (required) | yes | yes | `android://` picks the only connected device; `android://<serial>` picks a device by its adb serial; a path selects what to back up or where to restore |
| `serial` | yes | yes | Device serial, same as putting it in the location |
| `source_path` | yes | | Absolute device path to back up (overrides the location path). Default: the shared storage, usually `/storage/emulated/0` |
| `destination_path` | | yes | Absolute device directory to restore into (overrides the location path). Default: a new `plakar-restore-<timestamp>` directory at the top of the shared storage |
| `overwrite` | | yes | Replace existing files whose content differs (default `false`: they are reported as errors; identical size and mtime are skipped silently) |
| `allow_any_path` | | yes | Allow a destination outside `/storage`, `/sdcard`, `/mnt/sdcard`, `/data/local/tmp` (default `false`) |
| `adb_server` | yes | yes | `host:port` of the adb server (default `127.0.0.1:5037`, honours `ANDROID_ADB_SERVER_PORT` and `ANDROID_ADB_SERVER_ADDRESS` like adb) |
| `start_server` | yes | yes | Run `adb start-server` when no server answers on a local address (default `true`) |
| `streams` | yes | yes | Parallel transfers over the adb link, 1 to 16 (default `4`). USB is one cable: more streams rarely help |

Excludes use plakar's usual `-exclude` flag and syntax (gitignore-like,
negations with `!`).

## Examples

```bash
# back up the shared storage of the only connected device
$ plakar at /tmp/store backup android://

# several devices: pick one by serial (see "adb devices")
$ plakar at /tmp/store backup android://R58M12ABCDE

# only the camera roll, skipping the thumbnails
$ plakar at /tmp/store backup -exclude '**/.thumbnails' android://R58M12ABCDE/storage/emulated/0/DCIM

# same thing with options, as a named source
$ plakar source add phone android://R58M12ABCDE source_path=/storage/emulated/0/DCIM
$ plakar at /tmp/store backup @phone

# restore the whole snapshot into a fresh directory on the device
$ plakar at /tmp/store restore -to android://R58M12ABCDE <snapid>

# restore one directory where it was, replacing changed files
$ plakar at /tmp/store restore -to android://R58M12ABCDE/storage/emulated/0/DCIM -o overwrite=true <snapid>:/storage/emulated/0/DCIM
```

Snapshots keep the device paths, so a snapshot of the shared storage lists
as `/storage/emulated/0/DCIM/...`, `/storage/emulated/0/Download/...` and
so on; restoring `<snapid>:/storage/emulated/0/DCIM` writes the content of
`DCIM` directly into the destination.

## What is backed up

The default source is the device's primary shared storage, found by trying
`/storage/emulated/0`, `/sdcard`, `/storage/self/primary` and
`/mnt/sdcard`. On Android 11 and later the `Android/data` and `Android/obb`
subtrees are only partly readable over adb; the importer records what the
device lets it list and reports the rest as errors, it does not guess.

An external SD card (`/storage/XXXX-XXXX`) is backed up by naming it:
`android://<serial>/storage/XXXX-XXXX`.

Not backed up, by design: anything that needs root (`/data/data`, `/data`,
system partitions), and anything that is not a file (contacts, SMS, call
log, accounts, settings, app state). The *companion application* proposal
in `docs/companion-app.md` covers those.

Metadata: Android's shared storage is an emulated filesystem whose
ownership and permission bits are synthetic (everything is `root:sdcard_rw`
or `media_rw`, mode `0660`/`0771`). They are recorded as the device reports
them. Modification times are real and are preserved both ways.

## Device states and errors

| Situation | What plakar says |
|---|---|
| No device | `no Android device connected: enable USB debugging ...` |
| Several devices, no serial | `2 devices connected, specify one with android://<serial>: R58M12ABCDE (SM G973F) [device], emulator-5554 [device]` |
| Device shows `unauthorized` | `device X is not authorized: unlock it and accept the "Allow USB debugging?" prompt` |
| Device shows `offline` | `device X is offline: unplug and replug it, or revoke USB debugging authorizations ...` |
| `no permissions` (Linux udev) | `device X cannot be opened by the adb server (no permissions): ... add a udev rule` |
| Unknown serial | `device X not found; connected devices: ...` |
| adb server not running, adb not installed | `cannot reach the adb server ...; starting it failed: adb executable not found: install Android platform-tools ...` |

The device chosen at the start of a run is the only one used: if it
disconnects, the run fails, it never continues on another device.

Per-file problems (a file the device refuses to read, a directory it refuses
to list, a file that changed size while being read) are recorded as errors on
that path and the backup goes on. A transport failure (cable unplugged,
device locked and gone offline, adb server killed) aborts the backup with an
error: an interrupted run is never reported as a complete snapshot.

## Incremental snapshots

Every entry is recorded with the metadata the device reports (size, mtime,
mode, inode, ownership). On the next backup kloset compares it with the
previous snapshot and reuses the stored content of entries whose metadata is
unchanged, without transferring them. Only new and changed files cross the
USB link; enumeration still walks the whole tree, which is fast (metadata
only, one listing per directory).

Two files with the same size and mtime are therefore taken as identical.
That is the same assumption plakar makes for local filesystems. A camera
app that rewrites a photo in place without touching its mtime defeats it.

Devices running Android 6 or older only speak the v1 sync protocol, whose
sizes are 32-bit: their files are always re-read and sizes measured from
the content.

## Restore

The exporter writes files and directories under the destination, creating
missing directories, and sets each file's modification time. It is a
file-level restore: it does not restore apps, settings, or anything the
backup did not contain.

- Nothing on the device is overwritten unless asked: by default an existing
  file with different content is an error for that path (the rest of the
  restore proceeds), and `-o overwrite=true` replaces it. An existing file
  with the same size and mtime is left alone.
- The destination must lie in shared storage (`/storage/...`, `/sdcard`,
  `/mnt/sdcard`, `/data/local/tmp`) unless `-o allow_any_path=true` is
  given; the device root is always refused.
- Record paths are validated: a path that is not clean, or would land
  outside the destination, is rejected.
- Symlinks are attempted; Android's shared storage refuses them and the
  device's answer is reported for that path. Devices, pipes and sockets are
  refused.
- Permission bits and ownership are not restorable on shared storage (the
  emulated filesystem ignores them); directory mtimes are not restored.
- A failed transfer leaves no partial file: adbd removes the target when a
  push fails.

## Security

The device is treated as an untrusted filesystem:

- Every reply is parsed with bounded lengths; a malformed reply ends the
  connection with a protocol error.
- Directory entry names containing `/` or NUL, and paths longer than adb's
  1024-byte limit, are rejected.
- Symbolic links are never followed during enumeration; their target is
  recorded as data.
- The only shell commands ever run on the device are `readlink` (symlink
  targets) and `mkdir -p` (empty directories on restore), with every
  argument single-quoted. Nothing read from the device is ever interpolated
  into a command.
- The device is addressed by serial for every connection; the adb server
  is never asked for "any device".
- Serial numbers appear in error messages and as the snapshot origin, as
  they are the only stable identity of a device; file contents are never
  logged.
- Plakar never enables debugging, authorizes itself, or escalates: a
  device that is not authorized is reported, with the fix.

## Limitations

- Not a full device backup (see *What is backed up*).
- Throughput is bound by adb over USB: expect 20 to 40 MB/s on USB 2 links,
  more on USB 3 devices. Many small files are dominated by per-file
  round trips.
- `Android/data` and `Android/obb` are mostly unreadable over adb since
  Android 11.
- Android 6 and older: no incremental transfer, no error details on
  unreadable entries, no `stat` through symlinks.
- Directory mtimes and shared-storage permission bits are not restored.

## Manual validation procedure

With a device at hand, after `make build` and installing the package:

1. Plug the device in with a data cable; run `adb devices` and make sure it
   shows as `device`.
2. If it shows `unauthorized`, unlock the device and accept the
   *Allow USB debugging?* prompt, ticking *Always allow from this computer*.
3. Create a test directory on the device with representative files:
   `adb shell mkdir -p /sdcard/Download/plakar-test`, then push a photo, an
   empty file, a file with spaces, quotes and non-ASCII characters in its
   name, a file over 100 MB, and a nested directory with an empty
   subdirectory.
4. Back up: `plakar at /tmp/store backup android://<serial>`; list the
   snapshot with `plakar at /tmp/store ls <snapid>:/storage/emulated/0/Download/plakar-test`
   and compare sizes and mtimes with `adb shell ls -la`.
5. On the device, modify one file, add one and delete one.
6. Back up again. The run should report the modified and added files as
   transferred and the rest as cached (watch the counters in the progress
   output, or compare `plakar at /tmp/store info <snapid>` of both).
7. `plakar at /tmp/store diff <snap1> <snap2>` must list exactly the
   modified, added and deleted files.
8. Restore the test directory: `plakar at /tmp/store restore -to android://<serial> <snap2>:/storage/emulated/0/Download/plakar-test`.
   It lands in `/sdcard/plakar-restore-<timestamp>`.
9. Compare: `adb pull` both directories to the computer and `diff -r`
   them; check mtimes with `adb shell ls -la`.
10. Start a backup of the whole shared storage, unplug the cable half way:
    the backup must fail with a transport error and no snapshot must be
    committed (`plakar at /tmp/store ls` shows none for that run). Plug it
    back in and run the backup again; it must complete.

Finally delete the test data on the device: `adb shell rm -rf
/sdcard/Download/plakar-test /sdcard/plakar-restore-*`.

## Hardware tests

`make hwtest SERIAL=<serial>` runs the tests in `importer/hardware_test.go`
against a real device: device discovery, a full enumeration of the shared
storage (read-only), and a write/read round trip in a directory of its own
under `Download/plakar-hwtest-<timestamp>`, deleted afterwards. They never
touch existing files. `PLAKAR_ANDROID_TEST_SERVER=1 go test ./adb/` checks
the host protocol against a running adb server without any device.

## Developing without a device

`adb/adbtest/fakeadbd` is the test fake adbd as a standalone process. It
serves a fake device whose shared storage is a built-in sample, or a host
directory of your choice, and plakar can be pointed at it with `adb_server`:

```bash
$ go run ./adb/adbtest/fakeadbd -listen 127.0.0.1:5599 -dir ~/Pictures &
$ plakar at /tmp/store backup -o adb_server=127.0.0.1:5599 android://FAKE0001
$ plakar at /tmp/store restore -to android://FAKE0001/sdcard/restore -o adb_server=127.0.0.1:5599 <snapid>
```

`-legacy` makes it behave like an Android 6 device (v1 protocol only).

## Performance

`go test -bench . ./importer/` measures the protocol overhead against the
in-memory fake adbd (on an Apple M4 Pro: 270k entries/s enumeration,
195 MB/s for 4 KiB files, 4.7 GB/s for large files). These numbers have no
USB in them.

To measure on hardware, with the same dataset on the device:

```bash
time adb pull /sdcard/DCIM /tmp/pull                      # the baseline
time plakar at /tmp/store backup android://<serial>/sdcard/DCIM
time plakar at /tmp/store backup android://<serial>/sdcard/DCIM   # incremental
```

and `go test -run TestHardwareEnumerate -v ./importer/` for enumeration
throughput. Memory stays bounded whatever the tree: one directory listing
and `streams` 64 KiB chunks in flight.

No measurements on hardware are included here: none was available when
this integration was written.
