# iPhone integration: hardware test procedure

The unit tests run without a device. This is the manual pass against a real
one, to run before a release and after any change to `common/`. Budget an
afternoon for a loaded device; a test device with little data on it goes
through in minutes.

Use a device you can afford to wipe: step 7 restores onto it.

## Setup

1. Install libimobiledevice (`brew install libimobiledevice` on macOS,
   `apt install libimobiledevice-utils usbmuxd` on Debian/Ubuntu) and check:

   ```sh
   idevicebackup2 -v        # 1.3.0 or later
   ```

2. Build and install the plugin from this directory:

   ```sh
   make install
   plakar pkg list          # iphone listed
   ```

3. Create a throwaway repository and a staging directory on a volume with
   more free space than the device uses:

   ```sh
   plakar at /tmp/iphone-store create
   export WORKDIR=/Volumes/Staging/iphone
   ```

4. Plug the device in, unlock it. If it has never trusted this computer,
   **do not** accept the dialog yet: step 2 of the pass covers it.

## Pass

### 1. Discovery

```sh
idevice_id -l
```

The device's UDID is printed. Then, with a second device plugged in as well:

```sh
plakar at /tmp/iphone-store backup iphone://
```

Expected: an error naming both UDIDs and asking for `iphone://<udid>`.
Unplug the second device.

### 2. Pairing from an untrusted host

If the device already trusts the host, forget it first:

```sh
idevicepair unpair
```

Then:

```sh
plakar at /tmp/iphone-store backup iphone:// -o working_directory=$WORKDIR
```

Expected: the message "does not trust this host yet, requesting pairing"
and the Trust dialog on the device. Tap **Don't Trust** once: the run must
fail immediately with "denied the trust dialog". Run again, tap **Trust**,
enter the passcode: the run continues into the backup.

### 3. First (full) backup

Let the run from step 2 finish, or start it. On iOS 16 and later the device
asks for its passcode again when the backup starts.

Expected:

- "Backup Successful" from the tool, then "iphone: backup of <udid>
  finished (<name>, <model> <ios>, encrypted: <bool>)";
- `plakar at /tmp/iphone-store ls` shows one snapshot;
- `$WORKDIR/<udid>/Status.plist` reads `SnapshotState = finished`
  (`plutil -p` on macOS, `plistutil -i` elsewhere);
- the snapshot's file count matches the working directory:

  ```sh
  find $WORKDIR/<udid> -type f | wc -l
  plakar at /tmp/iphone-store ls -recursive <snapshot> | grep -vc '/$'
  ```

Note the duration and the size of `$WORKDIR/<udid>`.

### 4. Incremental backup

Change something on the device (take a photo, add a note), then:

```sh
plakar at /tmp/iphone-store backup iphone:// -o working_directory=$WORKDIR
```

Expected:

- "Incremental backup mode" from the tool, a run much shorter than step 3;
- a second snapshot; `plakar at /tmp/iphone-store diff <snap1> <snap2>`
  lists `Manifest.db`, `Status.plist`, `Manifest.plist`, `Info.plist` and
  the handful of hashed files that changed, nothing else.

### 5. Interrupted backup

Start another backup after a change on the device and unplug the cable
while files are being received.

Expected:

- the run fails with a backup error; `plakar ls` still shows two snapshots;
- `$WORKDIR/<udid>/Status.plist` no longer reads `finished`.

Plug the device back in and run the backup again.

Expected: it completes, a third snapshot exists, `Status.plist` is
`finished` again. If the device refuses and the tool reports an error on
every attempt, record it: this is the assumption in ARCHITECTURE.md about
repair of an interrupted working directory, and the fallback is
`-o full_backup=true`.

### 6. Restore to a directory

```sh
plakar at /tmp/iphone-store restore -to iphone:// -o working_directory=$WORKDIR <snap3>
```

Expected:

- "backup of <udid> ... written to $WORKDIR/restore/restore-XXXX" and the
  `idevicebackup2 ... restore` command, nothing sent to the device;
- the directory is accepted by the tool:

  ```sh
  idevicebackup2 info $WORKDIR/restore/restore-XXXX
  ```

  prints the device name, product version and backup date;
- it is identical to the working directory:

  ```sh
  diff -r $WORKDIR/<udid> $WORKDIR/restore/restore-XXXX/<udid>
  ```

### 7. Restore onto the device

**This replaces the data on the device and reboots it.**

```sh
plakar at /tmp/iphone-store restore -to iphone:// -o working_directory=$WORKDIR -o confirm=true <snap3>
```

Expected: "Restore Successful" from the tool, the device reboots, the photo
or note from step 4 is on it, the restore directory is gone.

Then the earlier state:

```sh
plakar at /tmp/iphone-store restore -to iphone:// -o working_directory=$WORKDIR -o confirm=true <snap1>
```

Expected: after the reboot the step 4 change is gone.

### 8. Encrypted backup

Turn on backup encryption on the device:

```sh
BACKUP_PASSWORD='test-password' idevicebackup2 encryption on
```

(confirm with the passcode on the device), then back up as in step 4.

Expected:

- "encrypted: true" in the integration's message;
- `Manifest.plist` reads `IsEncrypted = true`.

Restore it:

```sh
plakar at /tmp/iphone-store restore -to iphone:// -o working_directory=$WORKDIR -o confirm=true <snap4>
```

Expected: an error naming `backup_password`, nothing sent. Then:

```sh
BACKUP_PASSWORD='test-password' plakar at /tmp/iphone-store restore -to iphone:// -o working_directory=$WORKDIR -o confirm=true <snap4>
```

Expected: a successful restore. Check `ps` during the run: the password must
not appear in any command line.

Turn encryption back off when done:

```sh
BACKUP_PASSWORD='test-password' idevicebackup2 encryption off
```

### 9. Concurrency

Start a backup, and while it runs, in another terminal:

```sh
plakar at /tmp/iphone-store backup iphone:// -o working_directory=$WORKDIR
```

Expected: the second one fails at once with "another backup or restore of
this device is in progress"; the first finishes normally.

## Automated hardware test

The importer has a test that backs up the connected device and checks the
result; it runs only when asked:

```sh
PLAKAR_IPHONE_HARDWARE=1 PLAKAR_IPHONE_WORKDIR=$WORKDIR go test -v -run Hardware ./importer
```

`PLAKAR_IPHONE_UDID` selects the device when several are connected. There is
no automated restore test: restoring is destructive and stays manual.

## Record

Keep the results with the libimobiledevice version, the device model and iOS
version, the sizes and durations of steps 3 and 4, and anything that
deviated. They are the only evidence the integration works end to end.
