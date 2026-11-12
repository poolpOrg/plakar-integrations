# Proposal: a native MobileBackup2 implementation in Go

The MVP runs `idevicebackup2` and imports the directory it produces. This is
the case for, and the shape of, replacing it with Go code that speaks the
protocol itself and serves the device's filesystem requests from the
repository, so that no complete local copy of the backup is needed.

## What the protocol asks of the host

Verified from `src/mobilebackup2.c`, `src/device_link_service.c` and
`tools/idevicebackup2.c` in libimobiledevice 1.4.0.

**Transport.** usbmuxd exposes a Unix socket (`/var/run/usbmuxd`, a TCP port
on Windows) multiplexing TCP-like connections to ports on the device. The
host connects to lockdownd (port 62078), performs the pairing handshake
(host certificate and keys from the pair record, TLS session with the
device's certificate), and asks lockdownd to start
`com.apple.mobilebackup2`, which returns a port and whether the connection
is to be wrapped in TLS. The session must be started "with escrow bag" so
that a locked device accepts the backup. The notification proxy
(`com.apple.mobile.notification_proxy`) is observed for the user cancelling
on the device and the passcode prompts, and AFC (`com.apple.afc`) is used
only to hold `/com.apple.itunes.lock_sync` for the duration of the sync.

**Framing.** Messages are plists (binary, length-prefixed with a 32-bit
big-endian length) and raw byte streams. The session starts with the
DeviceLink handshake: `DLMessageVersionExchange` from the device, the host's
`DLMessageVersionExchange` reply with `DLVersionsOk`, then a `Hello`
message carrying the supported protocol versions (2.0, 2.1) answered with
the version the device picked.

**Requests.** The host sends one of `Backup`, `Restore`, `Info`, `List`,
`Unback`, `ChangePassword`, `EnableCloudBackup` with a `TargetIdentifier`
(and `SourceIdentifier`) UDID and an options dictionary (`ForceFullBackup`,
`RestoreSystemFiles`, `Password`, ...). From then on the **device** drives:

| message | the host must |
|---|---|
| `DLMessageDownloadFiles` | stream the named files from `<dir>/` to the device: for each, length-prefixed path, then chunks of `[len][0x0C][data]`, then `[1][0x00]` or `[len][0x06][errmsg]`; a zero length ends the list; then a status response, "Multi status" with the per-file errors if any |
| `DLMessageUploadFiles` | receive files: pairs of length-prefixed device path and host path, then chunks of `[len][0x0C][data]` ending with a zero length or `[len][0x0B][errmsg]`; write them under `<dir>/`; `Status.plist`, `Manifest.plist` and `Manifest.db` arrive this way |
| `DLContentsOfDirectory` | answer with a dictionary of entries: `DLFileType` (regular/directory), `DLFileSize`, `DLFileModificationDate` |
| `DLMessageCreateDirectory` | mkdir -p |
| `DLMessageMoveFiles` / `DLMessageMoveItems` | rename each source to its destination, replacing the destination |
| `DLMessageRemoveFiles` / `DLMessageRemoveItems` | remove each path, recursively for directories; a missing file is not an error |
| `DLMessageCopyItem` | copy a file or a directory tree |
| `DLMessageGetFreeDiskSpace` | report free bytes at the backup location |
| `DLMessagePurgeDiskSpace` | may refuse (the tool does) |
| `DLMessageProcessMessage` | the end of the operation, with `ErrorCode` and `ErrorDescription` |
| `DLMessageDisconnect` | end of session |

Paths in these requests are relative to the backup root and of the form
`<udid>/<hash>` or `<udid>/Snapshot/<hash>`, `<udid>/Manifest.db`, with
temporary names such as `<udid>/Manifest.db.tmp` and moves that promote them.
The device decides the sequence; the host is a filesystem with eight
operations.

## Where the local directory comes from

Apple's design assumes a host directory that persists between backups and
is mutated in place: the device uploads `Manifest.db`, lists what the host
has, asks for the previous manifest back (`DownloadFiles` of
`<udid>/Manifest.db` at the start of an incremental backup), uploads only
the changed files into `Snapshot/`, then issues the moves that merge
`Snapshot/` into the main tree and the removals of what the device deleted.
`idevicebackup2` serves all this from a real directory, which is why the
MVP needs a full copy on disk.

## A plakar-backed virtual directory

The eight operations map onto a mutable index over immutable content:

- **State** is a map from backup-relative path to either a plakar object
  (content already in the repository, identified by its MAC and size) or a
  file being written. It starts as the file list of the latest snapshot for
  this device, read from the snapshot's VFS, so the directory the device
  sees is the last backup.
- **`ContentsOfDirectory`** and **`GetFreeDiskSpace`** are answered from the
  index (sizes and mtimes are in the VFS entries).
- **`DownloadFiles`** reads the object from the repository and streams it.
  The device asks for little this way: the previous `Manifest.db` and the
  files it wants to verify. `Manifest.db` is tens of megabytes on a loaded
  device and is read in full.
- **`UploadFiles`** streams the incoming bytes straight into the snapshot
  being built, chunked and deduplicated as any importer record would be,
  under the device-given path. The index gets a "pending" entry for it.
- **`CreateDirectory`**, **`MoveFiles`**, **`RemoveFiles`**, **`CopyItem`**
  are index edits: move is a rename of keys, copy duplicates the entry
  (same object), remove drops keys.
- **`ProcessMessage` with `ErrorCode 0`** commits: the index is the file
  list of the new snapshot, which plakar seals. Anything else (an error, a
  cancellation, a lost connection) discards the pending snapshot and the
  index, and the repository is exactly as before.

The result: no host copy of the backup, incremental transfers preserved
(the device sees the previous manifest and files), and atomic commit for
free since plakar snapshots are only visible once sealed. Restore is the
mirror image: `Restore` is driven by `DownloadFiles` requests, served from
the chosen snapshot's VFS without materializing it.

### What the SDK would need

Today an importer produces a stream of records with lazily opened readers;
plakar pulls content from them and the importer has no handle on the
snapshot being built or on previous ones. The native implementation needs
three things an importer does not have:

1. **Reading a previous snapshot's files** during the import: the index is
   seeded from the last snapshot and `DownloadFiles` serves its content.
   Either the plugin opens the repository itself (it has the location and
   the key, but doing so from a plugin duplicates plakar's repository
   logic), or the importer protocol gains a "read from parent snapshot"
   request. The second is the right one and is a small addition to the
   gRPC service: `OpenParent(pathname) -> stream of chunks`, backed by the
   VFS cache plakar already keeps for incremental backups.
2. **Records whose pathname is decided late.** The device uploads into
   `Snapshot/<hash>` and renames afterwards. Either the import holds the
   content until the final name is known, which means spooling to disk and
   defeats the purpose, or the record stream allows "content under
   temporary ID X" followed by "X is now at path P" (and "X is dropped").
   kloset's snapshot builder already separates chunking (content, keyed by
   MAC) from the VFS entry (path); the record protocol does not expose the
   separation. This is the real change, in kloset's `snapshot.Builder` and
   the integration-sdk `Record`.
3. **A commit signal distinct from end-of-stream.** Today closing the record
   channel means success. The device decides success at the end, after the
   last `UploadFiles`; an importer needs to say "all records sent, and
   discard them" when `ProcessMessage` reports an error.

None of these exist in the SDK, which is why the MVP does not attempt this.

### The protocol itself in Go

- **usbmuxd client**: the socket protocol is a small plist-over-TCP
  exchange (`ListDevices`, `Connect`, `ReadPairRecord`); a few hundred
  lines. [jkcoxson/idevice](https://github.com/jkcoxson/idevice) has a
  clean Rust reference (`usbmuxd/`, `pairing_file.rs`).
- **lockdown**: `QueryType`, `GetValue`, `StartSession` with the pair
  record's host ID and system BUID, then TLS 1.2 with the pair record's host
  certificate and private key, then `StartService` with escrow bag. Go's
  `crypto/tls` handles the session; the pair record is read from usbmuxd,
  so pairing itself (generating certificates, the Trust dialog dance) can
  stay with `idevicepair` for a long while. `idevice`'s `lockdown.rs` and
  libimobiledevice's `lockdown.c` are the references.
- **DeviceLink and MobileBackup2** framing: as above, a few hundred lines
  on top of a binary plist codec (`howett.net/plist`, already a dependency).
- **notification_proxy** and **AFC lock**: optional for a first version but
  needed for parity (device-side cancel, passcode prompt reporting, and not
  colliding with Finder).

Estimated size: 3 to 4 thousand lines of Go with tests, against a protocol
whose only documentation is other people's implementations. Wire-level
tests can be recorded from a real session with `idevicebackup2 -d` and
replayed.

## Tradeoffs

| | `idevicebackup2` (MVP) | native Go |
|---|---|---|
| host disk | a full copy of the device | none beyond plakar's own cache |
| time per backup | transfer + import walk | transfer only |
| dependency | libimobiledevice installed, three tools in PATH | none (pairing still via `idevicepair` at first) |
| protocol risk | carried by a maintained project used by every Linux iOS tool | ours; iOS changes land on us |
| plakar changes | none | SDK: parent reads, late-bound record paths, discard; kloset: builder exposure |
| incremental | yes, Apple's own | yes, if the parent snapshot is served faithfully |
| restore | via the tool, from a materialized directory | served from the snapshot directly |
| failure mode | tool reports, directory possibly dirty, next run repairs | pending snapshot discarded, nothing to repair |
| browsing by original path | needs `Manifest.db` parsing either way | same |

## Recommendation

Do it in three steps, each shippable:

1. **Manifest.db browsing** (no protocol work): parse the SQLite index at
   import and emit, next to the hashed tree, a view by domain and original
   path, so `plakar ls` and restores of single files make sense. Encrypted
   backups need the key bag unwrapped with the password; keep that for
   last.
2. **SDK groundwork**: parent-snapshot reads and late-bound record paths
   with discard. These are useful beyond this integration (any importer
   that receives a stream it cannot name upfront, such as tar with renames,
   or database dumps with a final manifest).
3. **The native host**: usbmuxd, lockdown with an existing pair record,
   DeviceLink, MobileBackup2 over the virtual directory. Keep
   `idevicebackup2` as the fallback behind the same `iphone://` location
   (`engine=tool`) until the native path has been through the hardware
   procedure on several iOS versions.

Reimplementing pairing (certificate generation, the Trust dialog state
machine, the `SystemBUID` and `HostID` bookkeeping in usbmuxd's pair record
store) brings nothing to backups and should stay with libimobiledevice
indefinitely.
