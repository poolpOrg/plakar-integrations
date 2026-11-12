# iPhone integration: architecture

This is how the iphone importer and exporter are put together, what they
guarantee, and which of their assumptions about Apple's protocol are
verified against libimobiledevice's sources and which are not.

## Pieces

```
plakar backup iphone://              plakar restore -to iphone:// <snap>
        |                                        |
   iphone-importer (plugin)               iphone-exporter (plugin)
        |                                        |
   common.Tools  ---------------------------  common.Tools
        |                                        |
   idevicebackup2 backup                   idevicebackup2 restore
   idevicepair, idevice_id                 idevicepair, idevice_id
        |                                        |
     usbmuxd  <---------- USB ------------>  the device
```

- `common/` runs the three libimobiledevice tools through one `RunFunc`,
  validates backup directories, locks working directories.
- `importer/` is the plakar importer: find device, pair, back up, validate,
  walk.
- `exporter/` is the plakar exporter: write the snapshot into a directory,
  validate, optionally restore.
- `internal/faketools/` answers the tools' invocations in tests, including
  writing a backup directory shaped like the device's.

The plugins are ordinary integration-sdk plugins; plakar talks to them over
gRPC and `plakar pkg` packages them. Nothing in plakar or kloset was changed.

## Why idevicebackup2 and not the protocol

MobileBackup2 is a request/response protocol where the **device** drives the
session: once the host sends `Backup`, the device sends `DLMessage*`
requests (upload these files, list that directory, move, remove, copy, how
much space is free) and the host executes them against a local directory.
libimobiledevice's `idevicebackup2` implements the host side completely,
including the lockdown pairing, the escrow bag needed to start the service
on a locked device, the sync lock on the device and protocol version
negotiation (2.0 and 2.1). Reimplementing it was a bigger risk than the
value of not having a dependency, so the MVP runs the tool and owns
everything around it. The [native Go proposal](NATIVE-GO.md) is about
replacing it later.

## Importer lifecycle

1. **Construct** (`New`): parse the location and config, resolve the working
   directory (created if absent), list the devices (`idevice_id -l`) and
   select one. Any mistake stops here, before plakar opens a snapshot.
2. **Ping**: `idevicepair validate`; if the host is not trusted,
   `idevicepair pair` and retry it every 2 seconds up to `pairing_timeout`
   while the Trust dialog is up. A pairing the user denied stops at once.
3. **Import**:
   1. take the lock `<workdir>/.<udid>.lock` (an `flock`; `ErrLocked` if a
      backup or restore of that device is running);
   2. run `idevicebackup2 -u <udid> backup [--full] <workdir>` with its
      output streamed to stderr, so prompts ("waiting for passcode") reach the
      user;
   3. validate `<workdir>/<udid>` with `common.ValidateBackup`;
   4. emit `/`, then `/<udid>` and everything under it, regular files only.
4. **Close**: release the lock.

`Flags()` returns `FLAG_STREAM`. Without it plakar builds a second importer
and runs its `Import` to compute a progress summary, which would back up the
device twice.

`Origin()` is the UDID and `Root()` is `/`, so successive snapshots of a
device share the same `type:origin:root` key and plakar's VFS cache skips the
files whose metadata did not change. The root is `/` and not `/<udid>`
because a restore strips the snapshot root from the paths it hands the
exporter: with `/` as the root, `plakar restore -to /some/dir` produces
`/some/dir/<udid>/...`, exactly the directory `idevicebackup2 restore
/some/dir` reads.

## Consistency guarantees

**A snapshot holds a complete backup or does not exist.** `Import` returns
an error, and emits no record, unless `idevicebackup2` exited 0 *and*
`Status.plist` reads `SnapshotState = finished` *and* `Info.plist`,
`Manifest.plist` and `Manifest.db` are present *and* `Info.plist` names the
device. plakar does not commit a snapshot when its importer fails.

**An interrupted backup cannot corrupt the last good one.** The last good
backup lives in the repository, which is immutable; the integration only ever
writes to the working directory. An interruption leaves the working
directory in whatever state the device left it: `Status.plist` not
`finished`, partially updated files. That directory is never imported as is.
The next run hands it to the device again, which compares it against its
own state and brings it back to a consistent backup (the protocol was
designed for a directory that Finder/iTunes update in place across
interrupted syncs). If that ever fails, `full_backup=true` or deleting
`<workdir>/<udid>` gives the device a clean start, at the cost of a full
transfer.

**Two runs cannot interleave on the same working directory.** The lock is
taken before the tool starts and held until `Close`, since plakar may still
be reading record contents after `Import` returned. A crashed run does not
leave a stale lock: it is an `flock` on Unix and a `LockFileEx` range lock
on Windows, both dropped by the kernel with the process.

**Cancellation aborts the device session cleanly on Unix.** The tool is
started through `exec.CommandContext`; on cancellation it gets SIGINT, which
`idevicebackup2` handles by telling the device the sync is aborted and
unlocking the sync lock, then it has 10 seconds before it is killed. Windows
has no signal to deliver, so there the tool is killed and the device sees a
dropped connection; the next run repairs the working directory as after any
interruption.

**Secrets stay out of arguments and logs.** The only secret is the restore
password. It reaches `idevicebackup2` through `BACKUP_PASSWORD` in the
child's environment, is never part of a command line, and errors are built
from the tool's output only. Pairing records are libimobiledevice's business
and are never read.

## Exporter lifecycle

1. **Construct**: parse config; create `<workdir>/restore/restore-XXXX`
   and open it as an `os.Root`; with `confirm=true`, look the tools up and
   select the target device now, so a wrong UDID fails before anything is
   written.
2. **Export**: write every record under the staging root, through a
   temporary file renamed into place, with the record's mtime. Directories
   are created on the way. Symlinks and other special files do not occur in
   a device backup and are refused. Every failure is counted.
3. **Finish**, still inside `Export` once the record stream ends, so that
   its error is the restore command's error (plakar discards what `Close`
   returns):
   - any failed record: error, nothing is sent to a device, the directory
     is kept;
   - a snapshot restored as `<snapshot>:/<udid>` arrives without its
     `<udid>` directory; the manifests are then at the top of the staging
     directory and the files are moved under the UDID `Info.plist` names;
   - the directory must hold exactly one `<udid>` backup and pass
     `ValidateBackup`, the same checks as after a backup;
   - without `confirm`: print the directory and the `idevicebackup2 restore`
     command, and stop. This is the default because a device restore
     replaces the device's data and reboots it;
   - with `confirm`: refuse an encrypted backup without a password, make
     sure the target trusts the host, take the device lock, run
     `idevicebackup2 -u <target> -s <source> restore ... <staging>` with the
     password in the environment, and remove the staging directory on
     success. On failure it is kept for a retry by hand.

The source and target UDIDs can differ: Apple's restore supports moving a
backup to another device of a compatible model and iOS version.

## What is verified and what is assumed

Verified against libimobiledevice 1.4.0 (`tools/idevicebackup2.c`,
`tools/idevicepair.c`, `tools/idevice_id.c`, commit fa0f791 of June 2026)
and the installed binaries:

- `idevice_id -l` prints one UDID per line, USB devices only.
- `idevicepair validate`/`pair` exit 0 on success and 1 otherwise with the
  cause on stdout; "the user denied the trust dialog" and "accept the trust
  dialog ... then attempt to pair again" are the two messages the
  integration reacts to.
- `idevicebackup2 backup` writes `<dir>/<udid>/`, regenerates `Info.plist`
  on every run, runs incrementally whenever `<dir>/<udid>/Info.plist`
  exists, and forces a full transfer with `--full`.
- It exits 0 when the device reported `ErrorCode 0`, 255 for every local
  failure (no device, no directory, no lockdown) and `-ErrorCode` (mod 256)
  for a device error. It checks `Status.plist`'s `SnapshotState` for
  `finished` itself before restoring and prints "Backup Successful" only on
  that same condition after a backup.
- Encryption is decided by the device (`WillEncrypt` in the
  `com.apple.mobile.backup` lockdown domain); the backup command takes no
  password. Restore reads `BACKUP_PASSWORD` from the environment and also
  accepts `--password`, which this integration never uses.
- `Manifest.plist` carries `IsEncrypted`, which `idevicebackup2` reads to
  decide whether a password is needed.
- SIGINT makes the tool abort cleanly (`clean_exit` sets `quit_flag`).

Assumed, from the files real backups contain and from the community's
documentation of the format, not from Apple:

- `Manifest.plist` also carries `Version`, `Date` and a `Lockdown`
  dictionary with the device name, product type and iOS version. The
  integration only uses them for messages, with `Info.plist` as the primary
  source, and tolerates their absence.
- `Status.plist` carries `IsFullBackup`. Only used in messages and tests.
- A working directory left by an interrupted backup is repaired by the next
  backup run. That is how Finder and iTunes behave with the same protocol,
  and what `idevicebackup2` relies on, but it was not exercised against a
  device from this code.
- `Manifest.db` is present on iOS 10 and later and `Manifest.mbdb` before;
  either is accepted.

## Not tested against hardware

No device was available while this was written. Everything above the tools
was tested with a stand-in that reproduces their arguments, exit codes,
messages and on-disk layout. Still to be confirmed on a device, in the order
that matters:

1. a first full backup, then an incremental one, pass validation and the
   snapshot restores to a directory `idevicebackup2 info` accepts;
2. `plakar restore -o confirm=true` brings a device back;
3. the pairing flow from an untrusted host, including the passcode prompt on
   iOS 16 and later;
4. an interrupted backup followed by a successful one;
5. an encrypted backup and its restore with `BACKUP_PASSWORD`.

[HARDWARE-TESTING.md](HARDWARE-TESTING.md) is the procedure for all five.
