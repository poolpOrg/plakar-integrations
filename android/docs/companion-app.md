# Proposal: an Android companion application

Status: proposal, not scheduled. The USB integration (this directory) is
the MVP and stays the baseline; what follows is what it cannot do and how
a companion application would do it.

## Why

USB debugging gives a computer the files the device exposes to the
`shell` user: shared storage, and little else. It needs a cable, a
developer setting most people never turn on, and an authorization prompt.
It cannot see contacts, calendars, SMS, app data, or the media library's
metadata (albums, favourites, trashed items). It cannot run on a schedule
without the phone being plugged into a computer running plakar.

An application on the device can ask the user for exactly the data it
wants, through the platform's own permission model, and push it to a
plakar repository over the network. Nothing in it needs root.

## What the application would do

1. **Discover and read user data with the platform APIs**
   - MediaStore for photos, videos, audio and downloads, with their albums,
     dates, locations and favourite flags; it is also the only way to see
     media on Android 11+ without the broad `MANAGE_EXTERNAL_STORAGE`.
   - Storage Access Framework (`ACTION_OPEN_DOCUMENT_TREE`) for any
     directory the user picks, including SD cards and cloud providers'
     local folders, with persistable URI permissions so the choice survives
     reboots.
   - Contacts and calendar providers, exported as vCard and iCalendar
     files, behind their runtime permissions.
   - Application-specific exports where an app offers a share or export
     intent (messaging backups, password manager exports), driven by the
     user.
2. **Back up over the network**, Wi-Fi or mobile, to a plakar server
   (`plakar server`) or any storage plakar supports. Two options:
   - a thin client that streams a record stream to a `plakar` endpoint
     which does the chunking and storage server side (simplest on the
     phone, needs a reachable plakar);
   - kloset compiled for Android (gomobile) so the phone chunks, encrypts
     and writes to object storage itself, with no intermediary.
   The first is the MVP: it keeps the keys on the computer side and the
   phone-side code small.
3. **Schedule**: WorkManager jobs with constraints (charging, unmetered
   network, idle), the platform's answer to "back up every night".
4. **Change detection**: MediaStore's `generation` counters and
   `DATE_MODIFIED`, and SAF's `DocumentsContract` change notifications,
   give the list of changed items without walking the tree. The device
   keeps the last generation per volume; the backup reads only what moved.
   Content identity still comes from plakar's chunking, as today.
5. **Progress and control on the device**: a foreground service with a
   notification (required for long work anyway), a screen listing the
   last snapshots, their size and errors, and a button to run now.
6. **File-level restore** from the app: browse a snapshot, pick files,
   write them back through MediaStore (so they show in the gallery with
   their dates) or SAF.

## Security and privacy

- The app holds a token for one repository and one source name; the
  repository passphrase never leaves the computer in the thin-client
  design.
- Every data class is a separate permission the user grants in the app;
  the backup only contains what was granted.
- Transport is TLS to the plakar endpoint; the record stream is the same
  as the USB importer's, so the server side reuses the importer pipeline.
- No background access to the camera, microphone or location: MediaStore
  location metadata is read from the files, with the
  `ACCESS_MEDIA_LOCATION` permission.

## What it would reuse from this integration

- The record model and metadata mapping: the companion app emits the same
  paths (`/storage/emulated/0/DCIM/...`) so snapshots taken over USB and
  over Wi-Fi are comparable and incremental against each other.
- The exporter's policies (fresh destination by default, no silent
  overwrite) for the restore flow.
- The test strategy: a fake "device" that emits records, no hardware in CI.

## Rooted devices (separate, optional)

With root, adb can run `su` and read `/data/data` (application private
data), `/data/system` (accounts, settings), `/data/misc`, and the SMS and
call log databases. This is a different product with different risks:

- It must be an explicit, separate connector (`android+root://`), never a
  mode the USB integration slips into when it notices root.
- Consistency: app databases change under the backup; a correct backup
  stops the app (`am force-stop`) or snapshots the database with SQLite's
  backup API through a helper, not a raw file copy.
- Restore of private data is dangerous (uid mapping, SELinux labels,
  packages not installed); it should restore to a staging directory on the
  device and let the user or a helper apply it.
- Security: the `su` prompt, the data of every installed app in one
  snapshot, and the key material of apps make this a high-value target;
  it needs its own threat model and documentation.

It is worth prototyping once the companion app exists, since the same
rooted helper could expose a record stream to it. Not before.

## Effort

- Thin-client companion app, MediaStore and SAF only, manual backups:
  about four weeks for one developer who knows Android.
- Scheduling, progress, restore: two more weeks.
- Contacts and calendar: one week each, mostly UI for the permissions.
- Server-side endpoint accepting a record stream: already close to what
  the plugin protocol is; one week to expose it over TLS with tokens.
