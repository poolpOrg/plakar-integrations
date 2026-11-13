# Flipper Zero integration for plakar

Backs up, restores and stores klosets on a [Flipper Zero](https://flipperzero.one)
over the protobuf RPC the device exposes on its USB serial port. No app
has to be installed on the Flipper, the stock firmware is enough.

The device holds years of captured NFC and RFID tags, sub-GHz remotes,
infrared codes, iButton keys and BadUSB scripts on one microSD card that
nothing backs up. This integration makes that card, and the internal
flash with the settings and the dolphin state, a regular plakar source.

## Connectors

- **importer** walks a directory of the device and backs it up
- **exporter** restores a snapshot onto the device
- **storage** keeps a kloset on the device, for very small repositories
  and for the fun of it

## Locations

```
flipper://                         /ext on whichever Flipper is plugged in
flipper:///ext/nfc                 a directory on the SD card
flipper://int                      the internal flash
flipper://Lc4str1g/ext/subghz      a specific device when several are plugged in
```

`/ext` is the SD card, `/int` the internal flash. The device name is the
one printed on the device info screen or in the USB serial number, with
or without the `flip_` prefix.

The device is found on the USB bus by its vendor and product id. Set
`port` to a serial port path to bypass the detection:

```
$ plakar backup -o port=/dev/cu.usbmodemflip_Lc4str1g1 flipper://
```

## Examples

```bash
# back up the SD card
$ plakar at /var/backups/flipper backup flipper://

# back up only the NFC dumps and the settings
$ plakar at /var/backups/flipper backup flipper:///ext/nfc
$ plakar at /var/backups/flipper backup flipper://int

# what changed since last week
$ plakar at /var/backups/flipper diff <snapid1> <snapid2>

# restore a snapshot onto the device, or onto another Flipper
$ plakar at /var/backups/flipper restore -to flipper:///ext <snapid>
$ plakar at /var/backups/flipper restore -to flipper://OtherOne/ext <snapid>

# keep a kloset on the SD card
$ plakar at flipper:///ext/plakar create
$ plakar at flipper:///ext/plakar backup ~/notes
```

## Configuration

- `location` (required): `flipper://[device]/<path>`, defaults to `/ext`
- `port`: serial port of the device, autodetected over USB when omitted
- `md5` (importer, default `true`): ask the device for the MD5 of each
  file so that unchanged files are not transferred again

## Sharing the port

A serial port opens once, and plakar often runs two connector processes
against the same location at the same time (a command and the `cached`
daemon rebuilding its state, or a backup from the device into a kloset
kept on the device). The first process to open the port owns it and
serves the others over a unix socket in a private directory under the
temporary directory; the socket disappears with its owner.

## Limitations

The device reports neither modification times nor permissions. Files are
recorded with `0644`, directories with `0755`. With `md5` enabled every
file gets a fixed epoch modification time and its MD5 is kept in the
inode and device fields, which is what lets the backup cache skip files
that did not change. With `md5` disabled every backup re-reads
everything, which is slower but spares the device one hashing pass per
file.

The link is a USB CDC serial port and the RPC moves data in 512 byte
frames: a round-trip costs about 50 ms whatever the request, and bulk
transfers run at roughly 120 KB/s in both directions. The RPC has no
ranged read either: a blob fetch from the storage reads the whole
packfile, which the connector caches in memory for the next blobs. The
storage is a toy, a good one, not a backup target.

Symbolic links do not exist on the device and are reported as errors on
restore.

## Building

```bash
$ make build
$ make install     # packages and installs it in the local plakar
```
