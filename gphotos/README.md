# Google Photos Integration

## Overview

**Google Photos** is Google's photo and video storage service. This
integration backs up photos and videos from a Google Photos library into a
Kloset repository.

This integration provides:

- **Backup of picked photos and videos into a Kloset repository:** each item
  is downloaded at full quality (photos keep their Exif metadata, minus
  location) and stored with its original filename and creation time.
- **A date layout:** items are filed under `/YYYY/MM/` by creation date, so
  the snapshot browses like an archive. A flat layout is available too.
- **Incremental backups:** an item that was already backed up is recognised
  and not downloaded again.

> **Note:** Google Photos is import-only. The Google Photos APIs do not allow
> a third-party application to write arbitrary media back into a user's
> library, so there is no exporter.

> **Note:** This integration is interactive. Every backup needs someone at a
> browser to pick the photos, so it is not suited to scheduled, unattended
> backups.

## How picking works

Since March 2025, Google no longer lets third-party applications list a
user's Google Photos library. The only supported way to read media is the
**Google Photos Picker API**: the user selects the items in a Google Photos
web page, and the application receives what was selected.

A backup therefore goes like this:

1. `plakar backup gphotos://` opens a picker session and prints a URL.
2. You open that URL in a browser (signed in to the Google account), select
   the photos and videos to back up, and press **Done**.
3. plakar downloads the selection into the snapshot.

Google caps a session at **2000 items**. For a larger library, set
`sessions=N` to run N picking rounds in one backup: each round prints a new
URL, and an item picked twice is stored once. Items already present in a
previous snapshot are not downloaded again, so backing up a large library over
several runs is fine as well.

Two items with the same filename taken in the same month get a numeric suffix
(`IMG_0001 (1).jpg`), assigned in creation-time order. Picking a different set
of such duplicates can therefore shift the suffixes, and the shifted items are
stored again under their new name.

## Authentication

Google Photos only supports **user OAuth2**, so you need an OAuth client of
your own:

1. In the [Google Cloud console](https://console.cloud.google.com/apis/credentials),
   create a project, enable the **Google Photos Picker API**, and create an
   OAuth client of type **Desktop app**. Download its JSON.
2. On the OAuth consent screen, add your Google account as a test user
   (or publish the app). A refresh token issued to an app left in *Testing*
   expires after 7 days; when that happens the next backup runs the consent
   flow again and overwrites `token_file`.

On the first backup, point `credentials_file` at the downloaded JSON and
`token_file` at a path that does not exist yet. plakar prints a consent URL;
open it, grant access, and the token is saved to `token_file` for the
following runs.

## Configuration

The configuration parameters are as follows:

- `location` (required): always `gphotos://`
- `credentials_file`: path to the OAuth2 client JSON (Desktop app)
- `credentials_json`: the same JSON, inlined
- `token_file`: path to the OAuth2 token JSON; created by the consent flow if
  it does not exist
- `token_json`: the same token JSON, inlined
- `refresh_token`: a bare refresh token, used together with the client
  credentials
- `access_token`: a bare access token; it is **not** refreshed when it
  expires
- `sessions`: number of picking rounds (default `1`)
- `max_items`: maximum items per round, up to Google's cap of `2000`
  (default `2000`)
- `layout`: `date` (default) files items under `/YYYY/MM`, `flat` puts them
  all at the root
- `endpoint`: override the Picker API endpoint (testing only)

## Examples

```bash
# first run: grant access, then pick the photos to back up
$ plakar at /var/backups backup \
    -o credentials_file=~/.config/plakar/gphotos-client.json \
    -o token_file=~/.config/plakar/gphotos-token.json \
    gphotos://

# a large library: three picking rounds of up to 2000 items each
$ plakar at /var/backups backup \
    -o credentials_file=~/.config/plakar/gphotos-client.json \
    -o token_file=~/.config/plakar/gphotos-token.json \
    -o sessions=3 \
    gphotos://

# restore the backup to a local directory
$ plakar at /var/backups restore -to /tmp/photos <snapid>
```

The same options can be stored once with `plakar source add` so that the
backup command is just `plakar backup @photos`.
