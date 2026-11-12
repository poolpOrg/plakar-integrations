# Home Assistant integration

## Overview

The **Home Assistant** integration backs up and restores a Home Assistant
instance through its backup API, the one the Settings > System > Backups
page uses.  It works with every install type (Home Assistant OS, supervised,
container, core) on Home Assistant 2025.1 or later.

- `plakar backup homeassistant://host` asks the instance for a fresh backup,
  downloads it and stores its content in the kloset, file by file: the
  configuration directory, the recorder database, and on supervised installs
  the add-ons and shared folders.  Each file is deduplicated across snapshots.
- `plakar restore -to homeassistant://host <snapshot>` rebuilds the backup
  archive and uploads it to the instance, where it shows up in the Backups
  page.  With `restore=true` the instance restores it right away.

Inside the snapshot, `backup.json` sits at the root and every inner tar of the
backup is a directory: `homeassistant/data/` is the configuration directory,
`core_mosquitto/` an add-on, `share/` a shared folder.  Encrypted backups the
integration cannot open are kept as is (`homeassistant.tar.gz`).

## Configuration

- `location` (required): `homeassistant://host[:port]`, the port defaults
  to 8123.
- `token`: a long-lived access token of an administrator (Profile >
  Security).  Read from `PLAKAR_HOMEASSISTANT_TOKEN` when omitted; prefer the
  environment variable over a value on the command line.
- `tls`: reach the instance over HTTPS (default `false`).
- `agent`: the backup agent to use, the local one when omitted.

Importer only:

- `backup`: import this existing backup instead of making a new one.
- `name`: name of the backup made on the instance (`plakar <timestamp>`).
- `password`: encrypt the backup on the instance with this password; also
  used to read protected backups.
- `database`: include the recorder database (default `true`).
- `addons`: include every add-on, supervised installs only (default `true`).
- `folders`: shared folders to include, supervised installs only (default
  `media,share,ssl,addons/local`).
- `delete`: remove the backup from the instance once downloaded (default
  `false`).  The instance does not prune backups it did not schedule itself,
  so set it for a recurring job.  The removal happens before plakar commits
  the snapshot: should the run fail after the download, the backup has to be
  made again.

Exporter only:

- `password`: password of the backup, when its members were encrypted.
- `restore`: have the instance restore the backup once uploaded, which
  restarts it (default `false`).

The backup is spooled under the temporary directory (`TMPDIR`) while it is
read or assembled, so leave room there for about the size of the backup.

## Examples

```bash
# back up the instance, removing the backup from its disk afterwards
$ export PLAKAR_HOMEASSISTANT_TOKEN=eyJ...
$ plakar at /tmp/store backup -o delete=true homeassistant://homeassistant.local

# the configuration directory is browsable like any other snapshot
$ plakar at /tmp/store ls <snapid>:/homeassistant/data/

# upload a snapshot back as a backup, then restore it from the Backups page
$ plakar at /tmp/store restore -to homeassistant://homeassistant.local <snapid>

# or let the instance restore and restart right away
$ plakar at /tmp/store restore -o restore=true -to homeassistant://homeassistant.local <snapid>
```
