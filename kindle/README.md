# Kindle Integration

## Overview

**Kindle** is Amazon's family of e-readers.  When a Kindle is attached to a
computer over USB it exposes its storage as a filesystem, which is where the
books you side-loaded, your highlights and bookmarks, and the device's own
settings all live.

None of that is covered by Amazon's cloud sync: side-loaded books, annotations
on them, and `My Clippings.txt` exist only on the device.  If the Kindle is
lost, reset, or replaced, they are gone.

This integration allows:

* **Backup of a Kindle's contents into a Kloset repository:**
  Books, per-book annotation sidecars (highlights, bookmarks, reading position),
  clippings and, optionally, the device's collections and settings.

* **Restore of a snapshot back onto a Kindle:**
  Put a library back on a device after a factory reset, or move it onto a
  replacement device, with the annotations still attached to the right books.

* **Migration between devices:**
  Back up the old Kindle, restore onto the new one.

## Connecting the device

How a Kindle appears to the operating system depends on its generation.

**USB mass storage** — Kindles up to the Paperwhite 3, and the Basic through
its 10th generation, mount as a removable volume: `/Volumes/Kindle` on macOS,
`/media/<user>/Kindle` or `/run/media/<user>/Kindle` on Linux.  These are found
automatically.

**MTP** — the Paperwhite 4 onwards, the Oasis, the Scribe and the Colorsoft use
MTP instead.  MTP is not a filesystem, so it has to be mounted before this
integration can see it:

* *Linux*: gvfs mounts MTP devices under `$XDG_RUNTIME_DIR/gvfs`, which is
  searched automatically.  If gvfs is not available, `jmtpfs` or
  `simple-mtpfs` will mount the device on a path of your choosing, which you
  then pass as `location`.
* *macOS*: there is no MTP support in the OS.  Use a third-party MTP client to
  mount the device, or copy the tree off it, and point `location` at the
  result.

MTP mounts nest the Kindle's layout under a storage directory
(`Internal Storage`), which is looked through automatically, so the mount point
itself is the right thing to configure.

If the device is not found on Linux, it is usually permissions rather than the
Kindle: udev grants USB devices to the active local session, so over SSH the
node can end up read-only.  `ls -l /dev/bus/usb/*/*` and a rule matching the
device's vendor and product id sort that out.

## Installing

	$ git clone -b integration/kindle https://github.com/poolpOrg/plakar-integrations
	$ cd plakar-integrations/kindle
	$ make install

## Configuration

- `location` (required): `kindle://` to auto-detect the attached device, or
  `kindle:///path/to/mountpoint` to name it explicitly.  An explicit path is
  what you want when two Kindles are attached, when the device is mounted over
  MTP or FUSE, or when working from a copy of a Kindle tree on disk.

Importer only:

- `documents` (optional): back up the books, and the content the user put on the
  device themselves: `screenshots`, side-loaded `fonts`, `audible` audiobooks and
  `voice` data (defaults to `true`)
- `annotations` (optional): back up the `.sdr` sidecar directories, which hold
  highlights, bookmarks and reading positions, plus `My Clippings.txt` and, on a
  Scribe, the handwritten notebooks in `.notebooks` (defaults to `true`)
- `system` (optional): back up the device's `system` directory: collections and
  settings (defaults to `false`, since these describe one specific device)
- `everything` (optional): back up the whole volume verbatim, caches included,
  ignoring the options above (defaults to `false`)

Exporter only:

- `system` (optional): allow restoring into the device's `system` directory
  (defaults to `false`).  It is off by default because those files describe one
  particular device, and writing another Kindle's settings over them can
  confuse the firmware.

Thumbnails, search indexes, cloud indices and telemetry logs under `system` are
always excluded unless `everything` is set: they are large, they are rebuilt by
the device on demand, and they are meaningless on a different one.  Directory
names are matched case-insensitively, since the same directory is spelled
`Search Indexes` over MTP and `search indexes` over mass storage.

What a device exposes depends on how it is attached.  Over MTP a Scribe presents
its books under `documents/Downloads/Items01` rather than directly in
`documents`, so nothing here assumes a fixed depth.

## Examples

```bash
# Configure the attached Kindle as a source
$ plakar source add mykindle kindle://

# Back it up
$ plakar at /var/backups/kindle backup @mykindle

# Books only, leaving annotations behind
$ plakar source add books kindle:// annotations=false

# A faithful image of the whole volume, caches included
$ plakar source add kindlefull kindle:// everything=true

# A specific device, when several are attached
$ plakar source add oldkindle kindle:///Volumes/Kindle

# A device mounted over MTP on Linux
$ plakar source add scribe "kindle:///run/user/1000/gvfs/mtp:host=Amazon_Kindle_Scribe_G0923M10409300RQ"

# Restore onto a Kindle
$ plakar destination add newkindle kindle://
$ plakar at /var/backups/kindle restore -to @newkindle <snapid>
```

## Notes

Restores are written through a temporary file and renamed into place, so a
Kindle unplugged mid-transfer is left with the previous copy of a book rather
than a truncated one it would list but fail to open.

Writes are deliberately sequential: the device is on a slow bus with a FAT
filesystem, where concurrent writes buy nothing and fragment badly.

Ownership and permissions are normalised rather than preserved.  A Kindle's
filesystem carries neither, and what the operating system synthesises depends
on the mount options of whoever attached it, so normalising keeps snapshots of
the same device comparable across machines.

Books bought from Amazon are DRM-protected and tied to the account that bought
them.  They are backed up and restored faithfully, but a restored copy is
readable only on a device registered to that same account.
