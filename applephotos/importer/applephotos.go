/*
 * Copyright (c) 2026 Plakar
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

// Package importer implements the Apple Photos importer.
//
// A Photos library is a bundle directory: database/Photos.sqlite describes
// every asset and originals/ holds the files as imported, named after the
// asset's UUID. The importer reads both straight from disk, without going
// through Photos, so it works whether or not Photos is running. The database
// is in WAL mode and Photos may be writing to it, so rather than hold a read
// transaction on it for the whole backup the importer takes a consistent
// snapshot of it (VACUUM INTO) in a temporary directory and works off that.
//
// Only originals are backed up, under the filename they were imported with.
// Edits are not: Photos stores them as a recipe plus a rendered derivative,
// and the derivative is a cache that may not exist. The video half of a Live
// Photo is stored next to its still, as Photos does when exporting.
//
// With iCloud Photos and "Optimise Mac Storage" on, most originals are not
// on the Mac at all: the library only has a reduced copy. Those items are
// reported as errors so that the backup says what it could not include.
//
// Location syntax:
//
//	applephotos://                                    the default library
//	applephotos:///Users/me/Pictures/Family.photoslibrary
//
// The default library is ~/Pictures/Photos Library.photoslibrary. A
// relative path is taken from the working directory and a leading ~ is the
// home directory.
//
// Config keys (see plugin/importer/applephotos.json):
//
//	layout   "date" (default) files items under /YYYY/MM, "flat" puts them at the root
package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	_ "modernc.org/sqlite"
)

// Core Data stores dates as seconds since 2001-01-01 UTC.
var appleEpoch = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

const (
	kindVideo        = 1 // ZASSET.ZKIND
	subtypeLivePhoto = 2 // ZASSET.ZKINDSUBTYPE
)

// assetsQuery lists the assets that are not in Recently Deleted, oldest
// first so that items sharing a filename get stable suffixes.
const assetsQuery = `
SELECT a.ZUUID, a.ZDIRECTORY, a.ZFILENAME, a.ZDATECREATED, a.ZKIND, a.ZKINDSUBTYPE,
       b.ZORIGINALFILENAME, b.ZTIMEZONEOFFSET
FROM ZASSET a
LEFT JOIN ZADDITIONALASSETATTRIBUTES b ON b.ZASSET = a.Z_PK
WHERE a.ZTRASHEDSTATE = 0 OR a.ZTRASHEDSTATE IS NULL
ORDER BY a.ZDATECREATED, a.ZUUID`

type Importer struct {
	library  string // the .photoslibrary bundle
	hostname string
	layout   string // "date" or "flat"

	mu     sync.Mutex
	tmpdir string // holds the snapshot of the database
	db     *sql.DB
}

// busyTimeout is how long to wait for Photos to release the database when
// taking the snapshot.
const busyTimeout = 10 * time.Second

// asset is one row of assetsQuery.
type asset struct {
	uuid      string
	directory sql.NullString  // under originals/
	filename  sql.NullString  // <uuid>.<ext>
	created   sql.NullFloat64 // Core Data seconds, UTC
	kind      sql.NullInt64
	subtype   sql.NullInt64
	original  sql.NullString // filename at import time
	tzOffset  sql.NullInt64  // seconds east of UTC where the photo was taken
}

func init() {
	// Not FLAG_LOCALFS: with it kloset resolves an empty location to the
	// working directory before the importer sees it, which would leave no
	// way to ask for the default library. Relative paths are resolved
	// against the working directory here instead.
	integration.RegisterImporter("applephotos", 0, NewImporter)
}

func NewImporter(appCtx context.Context, opts *integration.Options, name string, config map[string]string) (integration.Importer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("apple photos: cannot find the home directory: %w", err)
	}
	cwd := ""
	if opts != nil {
		cwd = opts.CWD
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	library, err := resolveLibrary(strings.TrimPrefix(config["location"], name+"://"), home, cwd)
	if err != nil {
		return nil, err
	}
	if err := checkLibrary(library); err != nil {
		return nil, err
	}

	layout := config["layout"]
	if layout == "" {
		layout = "date"
	}
	if layout != "date" && layout != "flat" {
		return nil, fmt.Errorf("apple photos: unknown layout %q, want \"date\" or \"flat\"", layout)
	}

	hostname := ""
	if opts != nil {
		hostname = opts.Hostname
	}
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	return &Importer{
		library:  library,
		hostname: hostname,
		layout:   layout,
	}, nil
}

// resolveLibrary turns the path part of the location into the absolute
// path of the library bundle: empty means the default library, ~ the home
// directory, and a relative path is taken from cwd.
func resolveLibrary(loc, home, cwd string) (string, error) {
	loc = strings.TrimSpace(loc)
	switch {
	case loc == "":
		loc = filepath.Join(home, "Pictures", "Photos Library.photoslibrary")
	case loc == "~":
		loc = home
	case strings.HasPrefix(loc, "~/"):
		loc = filepath.Join(home, loc[2:])
	case !filepath.IsAbs(loc):
		if cwd == "" {
			return "", fmt.Errorf("apple photos: cannot resolve relative path %q", loc)
		}
		loc = filepath.Join(cwd, loc)
	}
	return filepath.Clean(loc), nil
}

// checkLibrary makes sure the path is a readable Photos library. On macOS
// the bundle is protected by TCC, so a permission error almost always
// means the terminal lacks Full Disk Access.
func checkLibrary(library string) error {
	st, err := os.Stat(library)
	if err != nil {
		return describe(library, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("apple photos: %s is not a Photos library", library)
	}
	dbPath := filepath.Join(library, "database", "Photos.sqlite")
	f, err := os.Open(dbPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("apple photos: %s is not a Photos library: no database/Photos.sqlite inside", library)
		}
		return describe(dbPath, err)
	}
	f.Close()
	return nil
}

func describe(p string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("apple photos: cannot read %s: %w (give the terminal Full Disk Access in System Settings > Privacy & Security)", p, err)
	}
	return fmt.Errorf("apple photos: cannot read %s: %w", p, err)
}

func (p *Importer) Origin() string        { return p.hostname }
func (p *Importer) Type() string          { return "applephotos" }
func (p *Importer) Root() string          { return "/" }
func (p *Importer) Flags() location.Flags { return 0 }

// Ping snapshots and opens the database, which is where a missing library
// or a terminal without Full Disk Access shows up.
func (p *Importer) Ping(ctx context.Context) error {
	db, err := p.open(ctx)
	if err != nil {
		return err
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM ZASSET").Scan(&n); err != nil {
		return fmt.Errorf("apple photos: cannot read the Photos database: %w", err)
	}
	return nil
}

// open takes a snapshot of the database into a temporary directory and
// opens it, once. VACUUM INTO runs in a single read transaction, so the
// snapshot is consistent even while Photos writes, and the live database is
// released as soon as it is done. The snapshot is removed by Close; one
// left behind by a killed plugin sits in the system temporary directory,
// which macOS clears on its own.
func (p *Importer) open(ctx context.Context) (*sql.DB, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.db != nil {
		return p.db, nil
	}

	tmpdir, err := os.MkdirTemp("", "plakar-applephotos-")
	if err != nil {
		return nil, fmt.Errorf("apple photos: cannot create a temporary directory: %w", err)
	}

	src := filepath.Join(p.library, "database", "Photos.sqlite")
	dst := filepath.Join(tmpdir, "Photos.sqlite")
	if err := snapshot(ctx, src, dst); err != nil {
		os.RemoveAll(tmpdir)
		return nil, err
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dst)+"?_pragma=query_only(1)")
	if err != nil {
		os.RemoveAll(tmpdir)
		return nil, fmt.Errorf("apple photos: cannot open the Photos database: %w", err)
	}
	db.SetMaxOpenConns(1)

	p.tmpdir = tmpdir
	p.db = db
	return db, nil
}

// snapshot copies the live database at src into dst as one transaction.
func snapshot(ctx context.Context, src, dst string) error {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", filepath.ToSlash(src), busyTimeout.Milliseconds())
	live, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("apple photos: cannot open the Photos database: %w", err)
	}
	defer live.Close()

	if _, err := live.ExecContext(ctx, "VACUUM INTO ?", filepath.ToSlash(dst)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("apple photos: cannot snapshot the Photos database: %w", err)
	}
	return nil
}

func (p *Importer) Import(ctx context.Context, records chan<- *integration.Record, _ <-chan *integration.Result) (map[string]string, error) {
	defer close(records)

	db, err := p.open(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, assetsQuery)
	if err != nil {
		return nil, fmt.Errorf("apple photos: cannot list the assets: %w", err)
	}
	defer rows.Close()

	dirs := make(map[string]bool)
	seenNames := make(map[string]int)

	if !p.emitDir(ctx, records, dirs, "/", time.Unix(0, 0).UTC()) {
		return nil, ctx.Err()
	}

	for rows.Next() {
		var a asset
		if err := rows.Scan(&a.uuid, &a.directory, &a.filename, &a.created, &a.kind, &a.subtype, &a.original, &a.tzOffset); err != nil {
			return nil, fmt.Errorf("apple photos: cannot read an asset: %w", err)
		}
		if !p.emit(ctx, records, dirs, seenNames, a) {
			return nil, ctx.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apple photos: cannot list the assets: %w", err)
	}
	return nil, nil
}

// createTime is when the photo was taken, in the time zone it was taken in
// when Photos knows it, so that the /YYYY/MM layout matches what Photos
// shows. The zero time means Photos has no date for the asset.
func (a asset) createTime() time.Time {
	if !a.created.Valid {
		return time.Time{}
	}
	t := appleEpoch.Add(time.Duration(a.created.Float64 * float64(time.Second)))
	if a.tzOffset.Valid {
		t = t.In(time.FixedZone("", int(a.tzOffset.Int64)))
	}
	return t
}

// name is the filename the asset was imported with, falling back to the
// name Photos gave the original on disk.
func (a asset) name() string {
	if a.original.Valid && a.original.String != "" {
		return a.original.String
	}
	if a.filename.Valid && a.filename.String != "" {
		return a.filename.String
	}
	return a.uuid
}

// originalPath is where the original sits in the bundle, or "" when the
// database does not say.
func (p *Importer) originalPath(a asset) string {
	if !a.directory.Valid || !a.filename.Valid || a.filename.String == "" {
		return ""
	}
	return filepath.Join(p.library, "originals", filepath.FromSlash(a.directory.String), a.filename.String)
}

// send hands a record to kloset, giving up when the backup is cancelled
// so that Import never blocks on a channel nobody drains anymore.
func send(ctx context.Context, records chan<- *integration.Record, r *integration.Record) bool {
	select {
	case records <- r:
		return true
	case <-ctx.Done():
		return false
	}
}

// emit sends the records for one asset: the directories it lives in (once),
// the original, and the video of a Live Photo. An original that is not on
// disk gets an error record instead. It returns false when the backup was
// cancelled.
func (p *Importer) emit(ctx context.Context, records chan<- *integration.Record, dirs map[string]bool, seenNames map[string]int, a asset) bool {
	created := a.createTime()

	dir := "/"
	if p.layout == "date" {
		if created.IsZero() {
			dir = "/undated"
			if !p.emitDir(ctx, records, dirs, dir, time.Unix(0, 0).UTC()) {
				return false
			}
		} else {
			year := "/" + created.Format("2006")
			if !p.emitDir(ctx, records, dirs, year, time.Date(created.Year(), 1, 1, 0, 0, 0, 0, time.UTC)) {
				return false
			}
			dir = year + "/" + created.Format("01")
			if !p.emitDir(ctx, records, dirs, dir, time.Date(created.Year(), created.Month(), 1, 0, 0, 0, 0, time.UTC)) {
				return false
			}
		}
	}

	name := uniqueName(seenNames, dir, a.name(), a.uuid)
	fullpath := path.Join(dir, name)

	src := p.originalPath(a)
	if src == "" {
		return send(ctx, records, integration.NewError(fullpath, errors.New("apple photos: the Photos database does not say where the original is")))
	}
	if !send(ctx, records, p.fileRecord(fullpath, name, a.uuid, src, created)) {
		return false
	}

	// The video half of a Live Photo sits next to the still as <uuid>_3.mov.
	if a.subtype.Valid && a.subtype.Int64 == subtypeLivePhoto {
		mov := strings.TrimSuffix(src, filepath.Ext(src)) + "_3.mov"
		movName := uniqueName(seenNames, dir, strings.TrimSuffix(name, path.Ext(name))+".mov", a.uuid+"_3")
		if !send(ctx, records, p.fileRecord(path.Join(dir, movName), movName, a.uuid+"_3", mov, created)) {
			return false
		}
	}
	return true
}

var errOffloaded = errors.New("apple photos: original is not on this Mac, only in iCloud Photos; download it in Photos or turn off \"Optimise Mac Storage\" to back it up")

// fileRecord describes the file at src, or explains why it cannot be read
// when it is not there: with iCloud Photos most originals are not.
func (p *Importer) fileRecord(fullpath, name, id, src string, created time.Time) *integration.Record {
	st, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return integration.NewError(fullpath, errOffloaded)
		}
		return integration.NewError(fullpath, describe(src, err))
	}

	mtime := created
	if mtime.IsZero() {
		mtime = st.ModTime()
	}
	fi := integration.FileInfo{
		Lname:    name,
		Lsize:    st.Size(),
		Lmode:    0644,
		LmodTime: mtime,
		Ldev:     1,
		Lino:     inode(id),
		Lnlink:   1,
	}
	return integration.NewRecord(fullpath, "", fi, nil, func() (io.ReadCloser, error) {
		return os.Open(src)
	})
}

func (p *Importer) emitDir(ctx context.Context, records chan<- *integration.Record, dirs map[string]bool, dir string, mtime time.Time) bool {
	if dirs[dir] {
		return true
	}
	dirs[dir] = true
	name := path.Base(dir)
	if dir == "/" {
		name = "/"
	}
	fi := integration.FileInfo{
		Lname:    name,
		Lmode:    os.ModeDir | 0755,
		LmodTime: mtime,
		Ldev:     1,
		Lino:     inode("dir:" + dir),
		Lnlink:   1,
	}
	return send(ctx, records, integration.NewRecord(dir, "", fi, nil, nil))
}

// uniqueName returns a filename unique within dir, case-insensitively,
// since nothing stops two imports from sharing a filename. Collisions get a
// numeric suffix; an item without a filename is named after its id.
func uniqueName(seen map[string]int, dir, filename, id string) string {
	name := filename
	if name == "" {
		name = id
	}
	name = strings.ReplaceAll(name, "/", "_")

	key := strings.ToLower(path.Join(dir, name))
	n, ok := seen[key]
	if !ok {
		seen[key] = 1
		return name
	}
	seen[key] = n + 1
	ext := path.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n, ext)
}

// inode derives a stable pseudo inode from an asset UUID so that kloset's
// cache recognises an unchanged item across backups.
func inode(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return h.Sum64()
}

// Close drops the snapshot of the database.
func (p *Importer) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var err error
	if p.db != nil {
		err = p.db.Close()
		p.db = nil
	}
	if p.tmpdir != "" {
		if rmErr := os.RemoveAll(p.tmpdir); rmErr != nil && err == nil {
			err = rmErr
		}
		p.tmpdir = ""
	}
	return err
}
