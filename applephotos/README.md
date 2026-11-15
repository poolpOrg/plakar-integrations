# Apple Photos Integration

## Overview

**Apple Photos** is the photo and video library of macOS, usually kept in
sync across devices by iCloud Photos. This integration backs up the photos
and videos of a Photos library into a Kloset repository.

This integration provides:

- **Backup of the originals into a Kloset repository:** each item is stored
  as it was imported into Photos, with its original filename, the metadata
  embedded in the file (Exif, location) and its capture time. The video half
  of a Live Photo is stored next to the still, as Photos does when exporting.
- **A date layout:** items are filed under `/YYYY/MM/` by capture date, in
  the time zone they were taken in, so the snapshot browses like Photos
  does. A flat layout is available too.
- **Incremental backups:** an item that was already backed up is recognised
  and not read again.

> **Note:** Apple Photos is import-only. Writing media back into a Photos
> library has to go through Photos itself, so there is no exporter.

## How it works

A Photos library is a bundle directory, by default
`~/Pictures/Photos Library.photoslibrary`. Inside, `database/Photos.sqlite`
describes every asset and `originals/` holds the files as imported. The
importer reads both directly, without going through Photos, so Photos does
not need to be running. It first takes a consistent snapshot of the database
into a temporary directory, so that Photos is free to keep writing during
the backup, and deletes the snapshot when the backup is done.

What is included:

- every photo and video in the library, hidden ones included
- the video of each Live Photo, named after the still with a `.mov` extension

What is not:

- items in **Recently Deleted**
- **edits**: Photos stores an edit as a recipe plus a rendered preview, and
  only the original is backed up
- originals that are **not on the Mac**: with iCloud Photos and *Optimise
  Mac Storage* on, the library keeps only a reduced copy of most items.
  Those are reported as errors in the backup, naming the item, so that the
  snapshot says what it could not include. To back them up, turn on
  *Download Originals to this Mac* in Photos > Settings > iCloud, or
  download the items you care about in Photos.

Two items with the same filename taken in the same month get a numeric
suffix (`IMG_0001 (1).HEIC`), assigned in capture-time order.

## Full Disk Access

macOS protects the Photos library: a program may not read inside the bundle
unless it has **Full Disk Access**. Grant it to the terminal application that
runs plakar (Terminal, iTerm, …) in **System Settings > Privacy & Security >
Full Disk Access**, then restart the terminal. Without it the backup fails
at once with a permission error that says so.

## Configuration

The configuration parameters are as follows:

- `location` (required): `applephotos://` for the default library, or
  `applephotos:///path/to/Library.photoslibrary`. A relative path is taken
  from the current directory and `~` stands for the home directory.
- `layout`: `date` (default) files items under `/YYYY/MM`, `flat` puts them
  all at the root

## Examples

```bash
# back up the default Photos library
$ plakar at /var/backups backup applephotos://

# back up another library
$ plakar at /var/backups backup "applephotos:///Volumes/Archive/Family.photoslibrary"

# restore the backup to a local directory
$ plakar at /var/backups restore -to /tmp/photos <snapid>
```

The location can be stored once with `plakar source add` so that the backup
command is just `plakar backup @photos`.
