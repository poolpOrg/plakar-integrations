package importer

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
)

// fixtureAsset describes one row of the fixture library.
type fixtureAsset struct {
	uuid     string
	original string    // ZORIGINALFILENAME; "" leaves the attributes row out
	ext      string    // extension of the file under originals/
	created  time.Time // zero leaves ZDATECREATED NULL
	tz       int       // ZTIMEZONEOFFSET, seconds east of UTC
	kind     int       // 0 photo, 1 video
	subtype  int       // 2 for a Live Photo
	trashed  bool
	content  string // "" means the original is not on disk (iCloud only)
	live     string // content of the Live Photo video, "" means none
}

// buildLibrary writes a Photos library bundle with just the tables and
// columns the importer reads.
func buildLibrary(t *testing.T, assets []fixtureAsset) string {
	t.Helper()

	lib := filepath.Join(t.TempDir(), "Test.photoslibrary")
	dbDir := filepath.Join(lib, "database")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dbDir, "Photos.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE ZASSET (
			Z_PK INTEGER PRIMARY KEY,
			ZUUID VARCHAR,
			ZDIRECTORY VARCHAR,
			ZFILENAME VARCHAR,
			ZDATECREATED TIMESTAMP,
			ZMODIFICATIONDATE TIMESTAMP,
			ZKIND INTEGER,
			ZKINDSUBTYPE INTEGER,
			ZTRASHEDSTATE INTEGER,
			ZHIDDEN INTEGER
		)`,
		`CREATE TABLE ZADDITIONALASSETATTRIBUTES (
			Z_PK INTEGER PRIMARY KEY,
			ZASSET INTEGER,
			ZORIGINALFILENAME VARCHAR,
			ZTIMEZONEOFFSET INTEGER
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	for i, a := range assets {
		dir := a.uuid[:1]
		file := a.uuid + a.ext
		var created any
		if !a.created.IsZero() {
			created = a.created.Sub(appleEpoch).Seconds()
		}
		trashed := 0
		if a.trashed {
			trashed = 1
		}
		if _, err := db.Exec(`INSERT INTO ZASSET (Z_PK, ZUUID, ZDIRECTORY, ZFILENAME, ZDATECREATED, ZMODIFICATIONDATE, ZKIND, ZKINDSUBTYPE, ZTRASHEDSTATE, ZHIDDEN)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			i+1, a.uuid, dir, file, created, created, a.kind, a.subtype, trashed); err != nil {
			t.Fatal(err)
		}
		if a.original != "" {
			if _, err := db.Exec(`INSERT INTO ZADDITIONALASSETATTRIBUTES (Z_PK, ZASSET, ZORIGINALFILENAME, ZTIMEZONEOFFSET) VALUES (?, ?, ?, ?)`,
				i+1, i+1, a.original, a.tz); err != nil {
				t.Fatal(err)
			}
		}

		origDir := filepath.Join(lib, "originals", dir)
		if a.content != "" {
			if err := os.MkdirAll(origDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(origDir, file), []byte(a.content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if a.live != "" {
			if err := os.MkdirAll(origDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(origDir, a.uuid+"_3.mov"), []byte(a.live), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return lib
}

func newTestImporter(t *testing.T, lib string, extra map[string]string) *Importer {
	t.Helper()
	config := map[string]string{"location": "applephotos://" + lib}
	for k, v := range extra {
		config[k] = v
	}
	opts := &integration.Options{Hostname: "mac.local", MaxConcurrency: 2, Stderr: io.Discard}
	imp, err := NewImporter(context.Background(), opts, "applephotos", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := imp.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return imp.(*Importer)
}

// drain runs Import and collects every record.
func drain(t *testing.T, imp *Importer) []*integration.Record {
	t.Helper()
	records := make(chan *integration.Record, 64)
	var got []*integration.Record
	done := make(chan struct{})
	go func() {
		for r := range records {
			got = append(got, r)
		}
		close(done)
	}()
	if _, err := imp.Import(context.Background(), records, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	return got
}

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestResolveLibrary(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	abs := filepath.Join(t.TempDir(), "Family.photoslibrary")

	tests := []struct {
		name string
		loc  string
		want string
	}{
		{"default", "", filepath.Join(home, "Pictures", "Photos Library.photoslibrary")},
		{"absolute", abs, abs},
		{"home", "~/Pictures/Family.photoslibrary", filepath.Join(home, "Pictures", "Family.photoslibrary")},
		{"relative", "Family.photoslibrary", filepath.Join(cwd, "Family.photoslibrary")},
		{"trailing slash", abs + string(filepath.Separator), abs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLibrary(tc.loc, home, cwd)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("resolveLibrary(%q) = %q, want %q", tc.loc, got, tc.want)
			}
		})
	}
}

func TestNewImporterRejects(t *testing.T) {
	lib := buildLibrary(t, nil)
	plain := t.TempDir()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		config map[string]string
		want   string
	}{
		{"missing", map[string]string{"location": "applephotos://" + filepath.Join(plain, "nope")}, "cannot read"},
		{"not a bundle", map[string]string{"location": "applephotos://" + plain}, "not a Photos library"},
		{"a file", map[string]string{"location": "applephotos://" + file}, "not a Photos library"},
		{"bad layout", map[string]string{"location": "applephotos://" + lib, "layout": "tree"}, "unknown layout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewImporter(context.Background(), &integration.Options{}, "applephotos", tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestImportDateLayout(t *testing.T) {
	lib := buildLibrary(t, []fixtureAsset{
		{uuid: "A1", original: "IMG_1.HEIC", ext: ".heic", created: date("2024-03-10T12:00:00Z"), tz: 3600, content: "still a"},
		{uuid: "B2", original: "img_1.heic", ext: ".heic", created: date("2024-03-11T12:00:00Z"), tz: 3600, content: "still b"},        // collides, case-insensitively
		{uuid: "C3", original: "IMG_2.MOV", ext: ".mov", created: date("2024-03-31T23:30:00Z"), tz: 7200, kind: 1, content: "video c"}, // April, local time
		{uuid: "D4", original: "IMG_3.HEIC", ext: ".heic", created: date("2023-12-25T08:00:00Z"), subtype: 2, content: "still d", live: "live d"},
		{uuid: "E5", original: "IMG_4.HEIC", ext: ".heic", created: date("2023-12-26T08:00:00Z")}, // in iCloud only
		{uuid: "F6", original: "IMG_5.HEIC", ext: ".heic", created: date("2023-12-27T08:00:00Z"), trashed: true, content: "deleted"},
		{uuid: "G7", ext: ".png", content: "undated g"}, // no attributes row, no date
		{uuid: "H8", original: "IMG_6.HEIC", ext: ".heic", created: date("2023-12-28T08:00:00Z"), subtype: 2, content: "still h"}, // Live Photo whose video is in iCloud only
	})
	imp := newTestImporter(t, lib, nil)
	records := drain(t, imp)

	var paths []string
	byPath := map[string]*integration.Record{}
	for _, r := range records {
		paths = append(paths, r.Pathname)
		byPath[r.Pathname] = r
	}
	sort.Strings(paths)
	want := []string{
		"/",
		"/2023", "/2023/12", "/2023/12/IMG_3.HEIC", "/2023/12/IMG_3.mov", "/2023/12/IMG_4.HEIC", "/2023/12/IMG_6.HEIC", "/2023/12/IMG_6.mov",
		"/2024", "/2024/03", "/2024/03/IMG_1.HEIC", "/2024/03/img_1 (1).heic",
		"/2024/04", "/2024/04/IMG_2.MOV",
		"/undated", "/undated/G7.png",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v\nwant %v", paths, want)
	}

	for _, p := range []string{"/", "/2023", "/2023/12", "/2024", "/2024/03", "/2024/04", "/undated"} {
		if !byPath[p].FileInfo.IsDir() {
			t.Errorf("%s should be a directory", p)
		}
	}

	for _, p := range []string{"/2023/12/IMG_4.HEIC", "/2023/12/IMG_6.mov"} {
		if err := byPath[p].Err; err == nil || !strings.Contains(err.Error(), "iCloud") {
			t.Errorf("%s: offloaded original should be reported as an iCloud error, got %v", p, err)
		}
	}

	fi := byPath["/2024/03/IMG_1.HEIC"].FileInfo
	if fi.Size() != int64(len("still a")) || !fi.Mode().IsRegular() || fi.Name() != "IMG_1.HEIC" {
		t.Errorf("unexpected file info %+v", fi)
	}
	if !fi.ModTime().Equal(date("2024-03-10T12:00:00Z")) {
		t.Errorf("mod time = %v", fi.ModTime())
	}
	if fi.Ino() == 0 || fi.Ino() == byPath["/2024/03/img_1 (1).heic"].FileInfo.Ino() {
		t.Errorf("inodes should be stable and distinct")
	}
	for _, r := range drain(t, newTestImporter(t, lib, nil)) {
		if r.Pathname == "/2024/03/IMG_1.HEIC" && r.FileInfo.Ino() != fi.Ino() {
			t.Errorf("inode changed across backups: %d then %d", fi.Ino(), r.FileInfo.Ino())
		}
	}

	for p, content := range map[string]string{
		"/2024/03/IMG_1.HEIC":     "still a",
		"/2024/03/img_1 (1).heic": "still b",
		"/2024/04/IMG_2.MOV":      "video c",
		"/2023/12/IMG_3.HEIC":     "still d",
		"/2023/12/IMG_3.mov":      "live d",
		"/undated/G7.png":         "undated g",
	} {
		r := byPath[p]
		if r.Err != nil {
			t.Errorf("%s: unexpected error %v", p, r.Err)
			continue
		}
		data, err := io.ReadAll(r.Reader)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(data) != content {
			t.Errorf("%s = %q, want %q", p, data, content)
		}
		r.Reader.Close()
	}
}

func TestImportFlatLayout(t *testing.T) {
	lib := buildLibrary(t, []fixtureAsset{
		{uuid: "A1", original: "one.jpg", ext: ".jpeg", created: date("2024-03-10T12:00:00Z"), content: "a"},
		{uuid: "B2", original: "two.jpg", ext: ".jpeg", created: date("2021-01-01T00:00:00Z"), content: "b"},
	})
	imp := newTestImporter(t, lib, map[string]string{"layout": "flat"})
	var paths []string
	for _, r := range drain(t, imp) {
		paths = append(paths, r.Pathname)
	}
	sort.Strings(paths)
	if got := strings.Join(paths, ","); got != "/,/one.jpg,/two.jpg" {
		t.Errorf("paths = %s", got)
	}
}

func TestDuplicateNamesAreSuffixedInCreationOrder(t *testing.T) {
	// Inserted newest first; suffixes must still follow creation time.
	lib := buildLibrary(t, []fixtureAsset{
		{uuid: "L1", original: "x.jpg", ext: ".jpeg", created: date("2024-03-20T12:00:00Z"), content: "later"},
		{uuid: "E1", original: "x.jpg", ext: ".jpeg", created: date("2024-03-01T12:00:00Z"), content: "earlier"},
	})
	imp := newTestImporter(t, lib, nil)
	names := map[string]string{}
	for _, r := range drain(t, imp) {
		if r.FileInfo.IsDir() {
			continue
		}
		data, _ := io.ReadAll(r.Reader)
		r.Reader.Close()
		names[r.Pathname] = string(data)
	}
	if names["/2024/03/x.jpg"] != "earlier" || names["/2024/03/x (1).jpg"] != "later" {
		t.Errorf("names = %v", names)
	}
}

func TestPing(t *testing.T) {
	lib := buildLibrary(t, []fixtureAsset{
		{uuid: "A1", original: "one.jpg", ext: ".jpeg", created: date("2024-03-10T12:00:00Z"), content: "a"},
	})
	imp := newTestImporter(t, lib, nil)
	if err := imp.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if imp.Origin() != "mac.local" || imp.Type() != "applephotos" || imp.Root() != "/" {
		t.Errorf("origin/type/root = %q %q %q", imp.Origin(), imp.Type(), imp.Root())
	}
}

func TestDatabaseCopyIsRemovedOnClose(t *testing.T) {
	lib := buildLibrary(t, nil)
	imp := newTestImporter(t, lib, nil)
	drain(t, imp)

	tmpdir := imp.tmpdir
	if tmpdir == "" {
		t.Fatal("Import should have copied the database")
	}
	if _, err := os.Stat(filepath.Join(tmpdir, "Photos.sqlite")); err != nil {
		t.Fatal(err)
	}
	if err := imp.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmpdir); !os.IsNotExist(err) {
		t.Errorf("temporary copy still there after Close: %v", err)
	}
}

func TestReadersOutliveImportContext(t *testing.T) {
	lib := buildLibrary(t, []fixtureAsset{
		{uuid: "A1", original: "one.jpg", ext: ".jpeg", created: date("2024-03-10T12:00:00Z"), content: "a"},
	})
	imp := newTestImporter(t, lib, nil)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record, 64)
	if _, err := imp.Import(ctx, records, nil); err != nil {
		t.Fatal(err)
	}
	cancel() // kloset is done with the Import RPC but still opens readers

	var file *integration.Record
	for r := range records {
		if !r.FileInfo.IsDir() {
			file = r
		}
	}
	data, err := io.ReadAll(file.Reader)
	file.Reader.Close()
	if err != nil {
		t.Fatalf("read after Import returned: %v", err)
	}
	if string(data) != "a" {
		t.Errorf("got %q", data)
	}
}

func TestImportStopsWhenCancelled(t *testing.T) {
	var assets []fixtureAsset
	for i := 0; i < 50; i++ {
		assets = append(assets, fixtureAsset{
			uuid: fmt.Sprintf("A%02d", i), original: fmt.Sprintf("%d.jpg", i), ext: ".jpeg",
			created: date("2024-03-10T12:00:00Z"), content: "x",
		})
	}
	lib := buildLibrary(t, assets)
	imp := newTestImporter(t, lib, nil)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record) // nobody reads: kloset went away
	errc := make(chan error, 1)
	go func() {
		_, err := imp.Import(ctx, records, nil)
		errc <- err
	}()
	<-records // the root directory
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Error("cancelled Import should report the context error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Import did not return after the context was cancelled")
	}
}
