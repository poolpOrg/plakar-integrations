# Minecraft server integration for plakar

Backs up and restores a Minecraft server directory: the worlds, the
server properties, the whitelist and bans, the plugins or mods and their
configuration, everything the server keeps on disk.

A running server rewrites its region files continuously, and a copy
taken while it does is a world with chunks from different moments that
may not load. This integration asks the server to hold its saves while
the backup runs, over the RCON console every server ships with:

1. `save-off`: autosave stops, the region files stop changing
2. `save-all flush`: everything loaded is written to disk
3. the directory is backed up like any other
4. `save-on`: autosave resumes, once the last file has been read

Players keep playing throughout; what they do during the backup stays in
memory until autosave resumes. A stopped server is backed up as is.

Restores refuse to write into a directory where a server is running.

## Connectors

- **importer** backs up the server directory
- **exporter** restores into it

Both are the fs connectors with the server-side care added, so excludes,
permissions, ownership and times behave the same.

## Enabling RCON

In `server.properties`:

```
enable-rcon=true
rcon.port=25575
rcon.password=choose-one
```

and restart the server. The console speaks plaintext, keep the port on
localhost or a private network. The integration reads these settings
from the server directory; `MINECRAFT_RCON_PASSWORD` in the environment
overrides the password, and `rcon` the address.

Without RCON a stopped server still backs up fine, and a running one is
refused rather than copied inconsistently.

## Examples

```bash
# back up the server
$ plakar at /var/backups/minecraft backup minecraft:///srv/minecraft

# the same from cron, the players stay in
$ plakar at /var/backups/minecraft backup -tag nightly minecraft:///srv/minecraft

# leave the logs out
$ plakar at /var/backups/minecraft backup -exclude 'logs/**' minecraft:///srv/minecraft

# restore the whole server, stopped, where it was
$ plakar at /var/backups/minecraft restore -to minecraft:///srv/minecraft <snapid>:/srv/minecraft/

# restore last night's overworld only
$ plakar at /var/backups/minecraft restore -to minecraft:///srv/minecraft <snapid>:/srv/minecraft/world

# bring the server up on another machine
$ plakar at s3://backups/minecraft restore -to minecraft:///srv/minecraft <snapid>:/srv/minecraft/
```

## Configuration

Importer:

- `location` (required): `minecraft://<server directory>`
- `quiesce` (default `true`): pause saving over RCON during the backup;
  with `false` the directory is copied as is, whatever the server does
- `rcon`: `host:port` of the console, from `server.properties` otherwise
- `timeout` (default `5m`): time allowed for each console exchange,
  `save-all flush` on a large world takes a while
- `dont_traverse_fs`: do not follow mount points under the directory

Exporter:

- `location` (required): `minecraft://<server directory>`
- `allow_running` (default `false`): restore even if a server runs
  there, for files the server does not hold open, such as a plugin
  configuration
- `rcon`, `timeout` (default `30s`): as above
- `skip_ownership`, `skip_permissions`, `skip_times`: as for `fs`

## How the server is detected

A server holds a lock on the `session.lock` of each world it has open,
released whenever it exits. The connectors look at that lock first, then
at whether something answers on the console. A backup refuses to run
when the server is up but cannot be told to pause, a restore refuses to
run when the server is up at all.

## Limitations

If plakar is killed between `save-off` and `save-on`, saving stays off
until someone runs `save-on` on the console. A cancelled backup turns
it back on by itself; a crash cannot.

A server in a container on Docker Desktop holds its world locks inside
the Docker VM, where the host cannot see them; there the connectors
only know the server is up through the console, so keep RCON enabled
and published.

Worlds on a Bukkit, Spigot or Paper server are three directories,
`world`, `world_nether` and `world_the_end`; restore them together.

## Tested with

Vanilla 26.3 in the `itzg/minecraft-server` image: backup of the live
server with the saves paused, restore refused while it ran, restore of
the snapshot into a new directory and a server booted from it with the
same seed and world.

## Building

```bash
$ make build
$ make install     # packages and installs it in the local plakar
```
