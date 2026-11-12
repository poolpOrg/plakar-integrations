# Android integration: architecture

## Data flow

```
 Android device                    computer
┌────────────┐  USB   ┌────────────┐  TCP 5037  ┌──────────────────────────┐
│ adbd       │◄──────►│ adb server │◄──────────►│ android importer/exporter│
│  sync:     │        │ (platform- │            │   adb client (this repo) │
│  shell:    │        │   tools)   │            │        │ io.Reader       │
└────────────┘        └────────────┘            │        ▼                 │
                                                │   kloset  ──► repository │
                                                └──────────────────────────┘
```

Plakar never touches USB. The adb server owns the transport, device
authorization and multiplexing; the integration is one more adb client,
like `adb pull`, speaking the documented host protocol over a local TCP
socket. That keeps the integration portable (the server exists for Linux,
macOS and Windows) and avoids fighting the adb server for the USB device.

## Packages

- `adb/`: the client. Host protocol framing (`client.go`), device list and
  feature parsing (`devices.go`), the SYNC protocol (`sync.go`: lstat,
  stat, list, recv, send, in v1 and v2 flavours), the shell protocol
  (`shell.go`: v2 with exit codes, v1 fallback), Linux mode and errno
  mapping (`mode.go`). No dependency beyond the standard library.
- `adb/adbtest/`: a fake adb server with fake devices and an in-memory
  Linux-like filesystem, with the real adbd's quirks (zero stat replies on
  v1, `.` and `..` in listings, `DONE` with no reason for an unreadable
  directory, FAIL mid-stream, symlink refusal). Fault injection: drop the
  connection during a listing or a transfer, fail a push, break the shell.
  Every test runs against it; nothing in CI needs hardware.
  `adb/adbtest/fakeadbd` runs it as a process so the plakar binary itself
  can be driven against a fake device (used to validate the packaged
  plugin end to end: backup, incremental backup, restore, diff).
- `common/`: configuration (`android://[serial][/path]` plus options),
  device selection and the error messages per device state, `adb
  start-server`, shared storage discovery, the containment rules for
  restore destinations, and the pool of sync sessions.
- `importer/`: enumeration and streaming reads.
- `exporter/`: file-level restore.
- `plugin/{importer,exporter}`: the plugin entry points and JSON schemas.

## Why a native client

The subset needed is small (host framing, five sync requests, one shell
request) and well specified by adb's own sources (`file_sync_protocol.h`,
`SERVICES.TXT`, `daemon/file_sync_service.cpp`). Existing Go libraries
either shell out to `adb`, are unmaintained, or are explicitly
work-in-progress; none was worth a dependency and a licence review for a
few hundred lines. A native client also makes the transport trivially
mockable: the fake server speaks the same bytes.

The v2 sync additions (`sendrecv_v2`) only add compression (brotli, LZ4,
zstd) and dry-run pushes. They were left out: they would add dependencies,
and kloset compresses anyway. `stat_v2` and `ls_v2` are used when the device
advertises them (Android 7+): 64-bit sizes, errno on failed entries,
ownership and inode numbers. Older devices get v1 with its 32-bit sizes;
their files are reported with an unknown size (`-1`) so kloset measures the
content rather than trusting a truncated number.

## Enumeration

One sync session lists directories depth first. Files are emitted while the
listing streams in; only the names of the subdirectories of the current
directory are kept until it is done, so memory is bounded by the depth of
the tree and the fan-out of one directory, not by the inventory. Entry
names come from the device and are validated (no `/`, no NUL, at most 255
bytes); paths are capped at adb's 1024-byte limit.

`LIST` is also used to detect unreadable directories: adbd answers an
`opendir` failure with a bare `DONE`, indistinguishable from an empty
directory, but a directory it could open always yields `.` and `..`. No
`.` means the directory could not be listed, which is reported as a
permission error on that path.

Symlinks are reported as such (lstat semantics), never followed. Their
target is read with `readlink --`, the one place the importer runs a shell
command, with the path single-quoted.

## Streaming reads

Records carry a lazy reader. When kloset opens it, a sync session is taken
from the pool (`streams` sessions at most, default 4), `RECV` is sent and
the `DATA` chunks (64 KiB each) are handed to kloset as they arrive. Memory
per transfer is one chunk. When the reader is closed after `DONE`, the
session goes back to the pool; a reader closed early leaves bytes on the
wire, so its session is marked broken and discarded.

The byte count is checked against the size from the listing: a file that
grew or shrank while being read is reported as an error for that path,
rather than stored with metadata that does not match its content.

## Error model

- Per-entry failures (errno on an entry, unreadable directory, FAIL on a
  read, size mismatch) become error records; the walk continues.
- Transport failures (the listing session dies, the device is gone) make
  `Import` return an error, which fails the backup. An interrupted run can
  never be committed as a complete snapshot.
- Cancellation closes the sockets through `context.AfterFunc`, which
  unblocks any pending read or write.

## Incremental snapshots

Nothing android-specific: kloset's VFS cache compares each record's
`FileInfo` (name, size, mode, mtime, dev, inode, uid, gid, nlink) with the
previous snapshot and reuses the stored content when they match. The
importer only has to report stable values, which `stat_v2` provides. The
round-trip test in `importer/roundtrip_test.go` backs up a fake device three
times through kloset and asserts that the second run reads no file and the
third reads exactly the modified and the added ones.

## Restore

Records are written with `SEND`, which creates parent directories and
replaces an existing regular file atomically from the client's point of
view (adbd unlinks on failure). Files are pushed concurrently through the
pool. Empty directories need a `mkdir -p`, batched a few dozen paths per
shell command at the end, all quoted.

Policies: the destination defaults to a fresh timestamped directory and
must lie in shared storage; existing files are errors unless unchanged or
`overwrite=true`; record paths are cleaned and confined to the destination.
Modification times are set through the `DONE` message; permission bits are
sent but the emulated filesystem ignores them.

## Concurrency

Enumeration is sequential on one session; adb listings are cheap and the
device, not the client, is the bottleneck. Content transfers use up to
`streams` sessions. Four is conservative on purpose: one USB cable, one
adbd, and the adb server multiplexes everything through it; the knob exists
so that USB 3 devices can be measured with more.

## Testing

- `adb/adb_test.go`: protocol framing against the fake and against raw
  listeners sending malformed replies (bad status, bad lengths, oversized
  chunks, entry names with slashes), cancellation, disconnection.
- `common/common_test.go`: URL and option parsing, device selection for
  every state, storage discovery, the pool.
- `importer/importer_test.go`: full tree with Unicode, spaces, commas and
  quotes in names, empty and multi-chunk files, excludes, sub-paths, a
  single file, permission errors, read errors, size changes, a dead
  listing, a vanished device, cancellation, device selection, v1 devices,
  bounded streams.
- `exporter/exporter_test.go`: tree restore with batched mkdir, default
  destination, destination containment, path traversal, overwrite policy,
  symlinks, partial failures, mkdir failure, a vanished device,
  cancellation.
- `importer/roundtrip_test.go`: three backups and a restore through kloset.
- `importer/hardware_test.go`: opt-in tests on a real device.
- `adb/realserver_test.go`: opt-in check of the framing against a real adb
  server, no device needed.
