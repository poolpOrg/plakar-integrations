# iPhone and iPad integration

Back up iPhones and iPads over USB into a plakar repository, and restore them,
without iCloud and without jailbreaking. The integration drives Apple's own
backup service on the device, the one Finder and iTunes use, through
[libimobiledevice](https://libimobiledevice.org/)'s `idevicebackup2`.

- **Importer** (`iphone://`, `iphone://<udid>`) runs a device backup into a
  persistent staging directory, checks that it finished, and imports the
  backup directory as it is. Backups are incremental: the device only sends
  what changed since the staging directory was last updated, and plakar only
  stores what changed since the last snapshot.
- **Exporter** (`iphone://`, `iphone://<udid>`) writes a snapshot back into a
  backup directory `idevicebackup2` can read and, when you say so, restores it
  onto a device.

What goes into the repository is the Apple backup as the device produced it:
`Info.plist`, `Manifest.plist`, `Manifest.db`, `Status.plist` and the content
files under their hashed names. If the device has backup encryption turned
on, the files are encrypted by the device before they reach the computer and
plakar stores them that way.

## Requirements

- libimobiledevice 1.3 or later with its tools in `PATH`: `idevice_id`,
  `idevicepair`, `idevicebackup2`.
  - macOS: `brew install libimobiledevice`
  - Debian and Ubuntu: `apt install libimobiledevice-utils usbmuxd`
  - Fedora: `dnf install libimobiledevice-utils usbmuxd`
  - Windows: builds of libimobiledevice plus Apple's USB driver (shipped with
    iTunes or "Apple Devices")
- On Linux the `usbmuxd` daemon must be running; it starts on its own when a
  device is plugged in on most distributions.
- A device running iOS 4 or later. Modern devices (iOS 10 and later) were the
  target; see "Limitations".

Plakar never embeds these tools: a missing one is reported with the install
hint when the integration starts.

## Installation

```sh
make install          # builds the plugin and runs plakar pkg add
```

or from a published package:

```sh
plakar pkg add iphone
```

## Pairing

A device has to trust the computer before it hands over a backup. The first
time you back up a device, unlock it and keep it unlocked: the integration
asks the device for a pairing, the device shows the **Trust This Computer?**
dialog, and the integration waits (60 seconds by default, `pairing_timeout`
to change it) for you to tap **Trust** and enter the passcode. Later runs
reuse the pairing record libimobiledevice stored on the computer.

To check or set up the pairing by hand:

```sh
idevice_id -l                 # UDIDs of the devices connected over USB
idevicepair validate          # is the device paired with this host?
idevicepair pair              # trigger the Trust dialog
```

## Backup

```sh
# the only connected device
plakar backup iphone://

# a specific device, when several are connected
plakar backup iphone://00008030-001A2B3C4D5E6F78

# as a configured source
plakar source add phone iphone://00008030-001A2B3C4D5E6F78 working_directory=/Volumes/Staging/iphone
plakar backup @phone
```

What happens:

1. The connected devices are listed. With no UDID in the location exactly
   one device must be connected; otherwise the run stops and lists them.
2. The pairing is validated, or requested (see "Pairing").
3. `idevicebackup2 backup` runs against the working directory. iOS 16 and
   later ask for the device passcode on the device before the backup starts;
   the tool's progress and prompts are shown as it runs.
4. The result is validated: `Status.plist` must say the backup finished, the
   manifests must be there and name the device. A backup that did not finish
   is reported as an error and **nothing** is imported.
5. The backup directory is imported as `/<udid>/...`.

Keep the device unlocked and plugged in for the whole run. A first backup
transfers everything on the device and can take an hour or more over USB;
later ones transfer only what changed.

### Options

| key                 | default                        | meaning |
|---------------------|--------------------------------|---------|
| `device_id`         |                                | UDID of the device, instead of putting it in the location |
| `working_directory` | `<user cache dir>/plakar/iphone` | where the backup is staged; keep it between runs |
| `full_backup`       | `false`                        | force a full backup instead of an incremental one |
| `pairing_timeout`   | `60s`                          | how long to wait for the Trust dialog |

`<user cache dir>` is `~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or
`~/.cache` on Linux, `%LocalAppData%` on Windows.

### The working directory

Apple's backup protocol works against a directory on the computer that it
updates in place: on every backup the device compares its state with the
`Manifest.db` in that directory and sends only what changed. The working
directory is therefore not a temporary file. It holds a full copy of the last
backup, which is what makes the next one incremental, and it needs as much
free space as the device uses. Losing it is not a data loss (the repository
has every snapshot) but the next backup is a full one again.

One backup or restore of a given device runs at a time against a working
directory; a second one started meanwhile fails at once rather than
corrupting the staging state.

### Encryption

Backup encryption is a property of the **device**, not of a backup run: when
it is on, every backup the device produces is encrypted with a key derived
from the backup password, and the password is needed only to restore. Turn
it on from Finder/iTunes, or with the device unlocked:

```sh
BACKUP_PASSWORD='the password' idevicebackup2 encryption on
```

Encrypted backups are the only ones that include saved passwords, Wi-Fi
settings, Health and HomeKit data; Apple's unencrypted backups leave those
out. Plakar does not need the password to back up and never asks for it.

## Restore

Restoring onto a device replaces its data and reboots it, so it is a two-step
operation by default.

```sh
# 1. write the backup directory, nothing touches the device
plakar restore -to iphone:// <snapshot>
```

This prints where the directory is and the `idevicebackup2` command that
restores it. Review it, then either run that command or let plakar do it:

```sh
# 2. restore onto the only connected device
plakar restore -to iphone:// -o confirm=true <snapshot>

# or onto another device than the one the backup came from
plakar restore -to iphone://<target-udid> -o confirm=true <snapshot>
```

For an encrypted backup the password is required; give it through the
environment so it stays out of the shell history and the process list:

```sh
BACKUP_PASSWORD='the password' plakar restore -to iphone:// -o confirm=true <snapshot>
```

### Options

| key                 | default | meaning |
|---------------------|---------|---------|
| `device_id`         |         | target UDID, instead of putting it in the location |
| `working_directory` | as importer | the backup directory is written under `<working_directory>/restore/` |
| `confirm`           | `false` | actually restore onto the device |
| `backup_password`   | `$BACKUP_PASSWORD` | password of an encrypted backup |
| `restore_system`    | `false` | restore system files too (`--system`) |
| `restore_settings`  | `false` | restore device settings (`--settings`) |
| `remove_items`      | `false` | remove items on the device that are not in the backup (`--remove`) |
| `no_reboot`         | `false` | do not reboot the device afterwards (`--no-reboot`) |
| `skip_apps`         | `false` | do not reinstall apps afterwards (`--skip-apps`) |
| `pairing_timeout`   | `60s`   | how long to wait for the Trust dialog |

Before anything is sent to the device the written directory is validated the
same way a backup is after it was taken, and `idevicebackup2` itself refuses
a backup whose `Status.plist` does not say it finished. After a successful
restore the written directory is removed; after a failure, or without
`confirm`, it is kept so the restore can be done or retried by hand. Each of
those runs leaves a device-sized directory under
`<working_directory>/restore/`; delete them once they have served.

A whole backup is required: a restore of a path inside it (for instance a
single hashed file) writes that file but is refused as a device restore,
because the device needs the manifests.

The snapshot can also be restored to a plain directory and handed to the
tool directly, which is the same thing the exporter does without `confirm`:

```sh
plakar restore -to /Volumes/Staging/restore <snapshot>
idevicebackup2 restore /Volumes/Staging/restore
```

## Browsing a backup

The snapshot holds Apple's layout, so files are under hashed names. The
mapping from a domain and path on the device to a hashed name is in
`Manifest.db`, a SQLite database (encrypted when the backup is). Browsing by
original path is a planned step, not something this version does.

## Limitations

- Tested against the protocol as implemented by libimobiledevice 1.4; devices
  older than iOS 10 use `Manifest.mbdb` instead of `Manifest.db` and were not
  tried.
- One device per snapshot. Back up each device separately.
- The device passcode has to be entered on the device for every backup on
  iOS 16 and later; there is no way around it.
- USB only. Wi-Fi sync is not supported.
- Full-device restores apply to the same iOS version or a newer one; Apple
  does not restore a backup onto an older iOS.
