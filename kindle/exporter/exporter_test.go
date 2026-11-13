package exporter_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/kindle/exporter"
	"github.com/poolpOrg/plakar-integrations/kindle/kindle"
)

// export pushes records through an exporter and returns the per-record errors.
func export(t *testing.T, config map[string]string, records []*integration.Record) map[string]error {
	t.Helper()

	exp, err := exporter.NewKindleExporter(context.Background(),
		&integration.Options{MaxConcurrency: 1}, "kindle", config)
	if err != nil {
		t.Fatalf("NewKindleExporter: %v", err)
	}
	defer exp.Close(context.Background())

	in := make(chan *integration.Record, len(records))
	for _, r := range records {
		in <- r
	}
	close(in)

	out := make(chan *integration.Result, len(records)+1)
	if err := exp.Export(context.Background(), nil, in, out); err != nil {
		t.Fatalf("Export: %v", err)
	}

	errs := make(map[string]error)
	for result := range out {
		errs[result.Record.Pathname] = result.Err
	}
	return errs
}

func fileRecord(pathname, content string) *integration.Record {
	return integration.NewRecord(pathname, "", integration.FileInfo{
		Lname: filepath.Base(pathname),
		Lsize: int64(len(content)),
		Lmode: 0o644,
	}, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	})
}

func dirRecord(pathname string) *integration.Record {
	return integration.NewRecord(pathname, "", integration.FileInfo{
		Lname: filepath.Base(pathname),
		Lmode: os.ModeDir | 0o755,
	}, nil, nil)
}

func kindleDest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, kindle.DocumentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestExportWritesFiles(t *testing.T) {
	dst := kindleDest(t)

	errs := export(t, map[string]string{"location": dst}, []*integration.Record{
		dirRecord("/documents/Papers"),
		fileRecord("/documents/Dune.azw3", "dune contents"),
		fileRecord("/documents/Papers/Paper.pdf", "paper"),
	})

	for pathname, err := range errs {
		if err != nil {
			t.Errorf("%s: %v", pathname, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dst, "documents", "Dune.azw3"))
	if err != nil {
		t.Fatalf("book not restored: %v", err)
	}
	if string(data) != "dune contents" {
		t.Errorf("contents = %q, want %q", data, "dune contents")
	}

	if _, err := os.Stat(filepath.Join(dst, "documents", "Papers", "Paper.pdf")); err != nil {
		t.Errorf("nested file not restored: %v", err)
	}
}

// parents get created even with no directory record first.
func TestExportCreatesMissingParents(t *testing.T) {
	dst := kindleDest(t)

	errs := export(t, map[string]string{"location": dst}, []*integration.Record{
		fileRecord("/documents/Deep/Nested/Book.azw3", "x"),
	})

	if err := errs["/documents/Deep/Nested/Book.azw3"]; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "documents", "Deep", "Nested", "Book.azw3")); err != nil {
		t.Errorf("file not restored into a created parent: %v", err)
	}
}

// writing into system/ needs an explicit opt-in.
func TestExportRefusesSystemByDefault(t *testing.T) {
	dst := kindleDest(t)

	errs := export(t, map[string]string{"location": dst}, []*integration.Record{
		fileRecord("/system/collections.json", "{}"),
	})

	if errs["/system/collections.json"] == nil {
		t.Error("restoring into system/ should fail without system=true")
	}
	if _, err := os.Stat(filepath.Join(dst, "system", "collections.json")); err == nil {
		t.Error("system file should not have been written")
	}
}

func TestExportAllowsSystemWhenEnabled(t *testing.T) {
	dst := kindleDest(t)

	errs := export(t, map[string]string{"location": dst, "system": "true"}, []*integration.Record{
		fileRecord("/system/collections.json", "{}"),
	})

	if err := errs["/system/collections.json"]; err != nil {
		t.Errorf("system=true should permit the write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "system", "collections.json")); err != nil {
		t.Errorf("system file should have been written: %v", err)
	}
}

// a snapshot carrying ".." must never write outside the device.
func TestExportRefusesTraversal(t *testing.T) {
	dst := kindleDest(t)
	outside := filepath.Join(filepath.Dir(dst), "escaped.txt")

	records := []*integration.Record{
		fileRecord("/../escaped.txt", "nope"),
		fileRecord("/documents/../../escaped.txt", "nope"),
	}

	export(t, map[string]string{"location": dst}, records)

	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a path traversal escaped the device root")
	}
}

// a short read must not leave a truncated book behind.
func TestExportRejectsShortWrite(t *testing.T) {
	dst := kindleDest(t)

	// claim 100 bytes but supply 4.
	record := integration.NewRecord("/documents/Truncated.azw3", "", integration.FileInfo{
		Lname: "Truncated.azw3",
		Lsize: 100,
		Lmode: 0o644,
	}, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("shrt")), nil
	})

	errs := export(t, map[string]string{"location": dst}, []*integration.Record{record})

	if errs["/documents/Truncated.azw3"] == nil {
		t.Error("a short write should be reported as an error")
	}

	// neither the target nor a temporary file should survive.
	if _, err := os.Stat(filepath.Join(dst, "documents", "Truncated.azw3")); err == nil {
		t.Error("truncated file should not have been left in place")
	}
	entries, err := os.ReadDir(filepath.Join(dst, "documents"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".plakar-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

// replacing a book is atomic: the old one stays readable until the new one is
// complete.
func TestExportOverwritesExistingBook(t *testing.T) {
	dst := kindleDest(t)
	target := filepath.Join(dst, "documents", "Dune.azw3")
	if err := os.WriteFile(target, []byte("old version"), 0o644); err != nil {
		t.Fatal(err)
	}

	errs := export(t, map[string]string{"location": dst}, []*integration.Record{
		fileRecord("/documents/Dune.azw3", "new version"),
	})
	if err := errs["/documents/Dune.azw3"]; err != nil {
		t.Fatalf("overwrite failed: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new version" {
		t.Errorf("contents = %q, want %q", data, "new version")
	}
}

// FAT has no symlinks; skipping must not fail the restore.
func TestExportSkipsSymlinks(t *testing.T) {
	dst := kindleDest(t)

	record := integration.NewRecord("/documents/link", "Dune.azw3", integration.FileInfo{
		Lname: "link",
		Lmode: os.ModeSymlink | 0o777,
	}, nil, nil)

	errs := export(t, map[string]string{"location": dst}, []*integration.Record{record})
	if err := errs["/documents/link"]; err != nil {
		t.Errorf("skipping a symlink should not be an error: %v", err)
	}
}

// records already carrying an error, and xattrs, pass straight through.
func TestExportPassesThroughErrorsAndXattrs(t *testing.T) {
	dst := kindleDest(t)

	records := []*integration.Record{
		integration.NewError("/documents/Unreadable.azw3", io.ErrUnexpectedEOF),
		integration.NewXattr("/documents/Dune.azw3", "user.comment",
			integration.AttributeExtended, func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("x")), nil
			}),
	}

	errs := export(t, map[string]string{"location": dst}, records)
	for pathname, err := range errs {
		if err != nil {
			t.Errorf("%s should pass through cleanly, got %v", pathname, err)
		}
	}
}

func TestExporterRejectsNonKindle(t *testing.T) {
	_, err := exporter.NewKindleExporter(context.Background(),
		&integration.Options{}, "kindle", map[string]string{"location": t.TempDir()})
	if err == nil {
		t.Error("restoring onto a directory that is not a Kindle should be refused")
	}
}

func TestExporterMetadata(t *testing.T) {
	dst := kindleDest(t)

	exp, err := exporter.NewKindleExporter(context.Background(),
		&integration.Options{}, "kindle", map[string]string{"location": dst})
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close(context.Background())

	if got := exp.Type(); got != "kindle" {
		t.Errorf("Type() = %q, want kindle", got)
	}
	if got := exp.Root(); got != "/" {
		t.Errorf("Root() = %q, want /", got)
	}
	if err := exp.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}
