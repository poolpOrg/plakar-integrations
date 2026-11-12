# Nextcloud integration for plakar

Backs up and restores the files of a [Nextcloud](https://nextcloud.com)
user with [plakar](https://plakar.io), with the trash bin and the kept
versions of every file when asked to.  It talks to the server's WebDAV
and OCS endpoints, so nothing has to be installed on the Nextcloud side.


## Installation

Build the plugin and install it into the local plakar:

	$ make install

`make build` alone produces the `nextcloud-importer` and
`nextcloud-exporter` binaries; `make test` runs the tests.


## Configuration

- `location` (required): `nextcloud://host[:port][/folder]`, the folder
  being relative to the user's files and the whole of them by default
- `username` (required): the login name
- `password` (required): an app password, see Settings > Security
- `prefix`: the URL path Nextcloud is installed under, for example
  `/nextcloud`; defaults to `/`
- `insecure`: set to `true` to talk plain HTTP to the server
- `trash` (importer only): set to `true` to also back up the trash bin
- `versions` (importer only): set to `true` to also back up the kept
  versions of every file


## Layout

A snapshot mirrors the DAV namespace of the server:

	/files/...                       the user's files
	/trashbin/<name>.d<timestamp>    deleted files, as Nextcloud names them
	/versions/<path>/<timestamp>     kept versions of /files/<path>

Restoring `<snapshot>:/files` to a nextcloud destination puts the files
back in place.


## Examples

Back up everything, trash bin and versions included:

	$ plakar source add nextcloud nextcloud://cloud.example.org username=alice password=xxxxx-xxxxx-xxxxx-xxxxx-xxxxx trash=true versions=true
	$ plakar backup @nextcloud

Back up a single folder of an instance installed under /nextcloud:

	$ plakar source add nextcloud nextcloud://example.org/Documents prefix=/nextcloud username=alice password=xxxxx-xxxxx-xxxxx-xxxxx-xxxxx
	$ plakar backup @nextcloud

Restore the files into a `Restored` folder of another account:

	$ plakar destination add nextcloud nextcloud://cloud.example.org/Restored username=bob password=xxxxx-xxxxx-xxxxx-xxxxx-xxxxx
	$ plakar restore -to @nextcloud <snapshot>:/files


## Limitations

* Only the files of the authenticated user are backed up.  Nextcloud does
  not let an administrator read other users' files over WebDAV, so an
  instance with several users needs one source per user.
* `versions=true` costs one request per file on top of the walk.
* Symlinks have no WebDAV counterpart: the exporter reports them as
  errors rather than guessing.
* Files are uploaded in a single PUT.  A reverse proxy with a request
  body limit will refuse files above it.
* Shares, tags, comments and favorites are not backed up.
