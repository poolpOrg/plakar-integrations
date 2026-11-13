package importer_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/kindle/exporter"
	"github.com/poolpOrg/plakar-integrations/kindle/importer"
	"github.com/poolpOrg/plakar-integrations/kindle/kindle"
)

// collect runs an importer to completion and returns what it produced.
func collect(t *testing.T, config map[string]string) (map[string][]byte, map[string]os.FileMode, []string) {
	t.Helper()

	opts := &integration.Options{Hostname: "testhost", MaxConcurrency: 1}

	imp, err := importer.NewKindleImporter(context.Background(), opts, "kindle", config)
	if err != nil {
		t.Fatalf("NewKindleImporter: %v", err)
	}
	defer imp.Close(context.Background())

	if err := imp.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	records := make(chan *integration.Record, 256)
	results := make(chan *integration.Result, 256)

	var importErr error
	go func() {
		_, importErr = imp.Import(context.Background(), records, results)
	}()

	contents := make(map[string][]byte)
	modes := make(map[string]os.FileMode)
	var errs []string

	for record := range records {
		if record.Err != nil {
			errs = append(errs, record.Pathname)
			continue
		}
		modes[record.Pathname] = record.FileInfo.Lmode
		if record.FileInfo.Lmode.IsRegular() {
			data, err := io.ReadAll(record.Reader)
			if err != nil {
				t.Fatalf("read %s: %v", record.Pathname, err)
			}
			record.Close()
			contents[record.Pathname] = data
		}
	}

	if importErr != nil {
		t.Fatalf("Import: %v", importErr)
	}

	return contents, modes, errs
}

func paths(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestImportDefaultSelection(t *testing.T) {
	root := kindle.NewTestVolume(t)

	contents, modes, errs := collect(t, map[string]string{"location": root})
	if len(errs) != 0 {
		t.Errorf("unexpected scan errors: %v", errs)
	}

	want := []string{
		"/documents/Dune.azw3",
		"/documents/Dune.sdr/Dune.mbp1",
		"/documents/Dune.sdr/assets/cover.jpg",
		"/documents/My Clippings.txt",
		"/documents/Papers/Paper.pdf",
	}

	got := paths(contents)
	if len(got) != len(want) {
		t.Fatalf("imported files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("imported[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if string(contents["/documents/Dune.azw3"]) != "dune contents" {
		t.Errorf("book contents not preserved: %q", contents["/documents/Dune.azw3"])
	}

	// directories are recorded too so a restore can recreate the tree.
	for _, dir := range []string{"/", "/documents", "/documents/Dune.sdr", "/documents/Papers"} {
		mode, ok := modes[dir]
		if !ok {
			t.Errorf("directory %q was not imported", dir)
			continue
		}
		if !mode.IsDir() {
			t.Errorf("%q mode = %v, want a directory", dir, mode)
		}
	}

	// normalised so snapshots taken on different hosts compare equal.
	if mode := modes["/documents/Dune.azw3"]; mode != 0o644 {
		t.Errorf("file mode = %v, want 0644", mode)
	}
	if mode := modes["/documents"]; mode.Perm() != 0o755 {
		t.Errorf("dir perm = %v, want 0755", mode.Perm())
	}
}

func TestImportSystemSelection(t *testing.T) {
	root := kindle.NewTestVolume(t)

	contents, _, _ := collect(t, map[string]string{
		"location":    root,
		"documents":   "false",
		"annotations": "false",
		"system":      "true",
	})

	got := paths(contents)
	want := []string{"/system/collections.json"}

	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("imported = %v, want %v (thumbnails and indexes must be excluded)", got, want)
	}
}

func TestImportEverything(t *testing.T) {
	root := kindle.NewTestVolume(t)

	contents, _, _ := collect(t, map[string]string{
		"location":   root,
		"everything": "true",
	})

	// caches and the partial download are all there in a verbatim copy.
	for _, p := range []string{
		"/system/thumbnails/thumb_Dune.jpg",
		"/system/search indexes/Dune.azw3.stmp",
		"/documents/half-download.azw3.tmp",
	} {
		if _, ok := contents[p]; !ok {
			t.Errorf("%q should be imported with everything=true", p)
		}
	}
}

func TestImporterRejectsEmptySelection(t *testing.T) {
	root := kindle.NewTestVolume(t)

	_, err := importer.NewKindleImporter(context.Background(),
		&integration.Options{}, "kindle", map[string]string{
			"location":    root,
			"documents":   "false",
			"annotations": "false",
			"system":      "false",
		})
	if err == nil {
		t.Error("an empty selection should be rejected rather than produce an empty backup")
	}
}

func TestImporterRejectsBadBool(t *testing.T) {
	root := kindle.NewTestVolume(t)

	_, err := importer.NewKindleImporter(context.Background(),
		&integration.Options{}, "kindle", map[string]string{
			"location":  root,
			"documents": "maybe",
		})
	if err == nil {
		t.Error("an unparseable boolean should be rejected")
	}
}

func TestImporterRejectsNonKindle(t *testing.T) {
	_, err := importer.NewKindleImporter(context.Background(),
		&integration.Options{}, "kindle", map[string]string{
			"location": t.TempDir(),
		})
	if err == nil {
		t.Error("a directory that is not a Kindle should be rejected")
	}
}

func TestImporterOriginUsesSerial(t *testing.T) {
	root := kindle.NewTestVolume(t)
	info := filepath.Join(root, kindle.SystemDir, ".device_info")
	if err := os.WriteFile(info, []byte(`{"serial":"G000AB01234567"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	imp, err := importer.NewKindleImporter(context.Background(),
		&integration.Options{Hostname: "testhost"}, "kindle",
		map[string]string{"location": root})
	if err != nil {
		t.Fatal(err)
	}
	defer imp.Close(context.Background())

	if got := imp.Origin(); got != "G000AB01234567" {
		t.Errorf("Origin() = %q, want the device serial", got)
	}
	if got := imp.Type(); got != "kindle" {
		t.Errorf("Type() = %q, want kindle", got)
	}
	if got := imp.Root(); got != "/" {
		t.Errorf("Root() = %q, want /", got)
	}
}

// TestRoundTrip backs up a device and restores it onto a second one.
func TestRoundTrip(t *testing.T) {
	src := kindle.NewTestVolume(t)

	// a destination that is a Kindle but otherwise empty.
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, kindle.DocumentsDir), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := &integration.Options{Hostname: "testhost", MaxConcurrency: 1}

	imp, err := importer.NewKindleImporter(context.Background(), opts, "kindle",
		map[string]string{"location": src})
	if err != nil {
		t.Fatalf("NewKindleImporter: %v", err)
	}
	defer imp.Close(context.Background())

	exp, err := exporter.NewKindleExporter(context.Background(), opts, "kindle",
		map[string]string{"location": dst})
	if err != nil {
		t.Fatalf("NewKindleExporter: %v", err)
	}
	defer exp.Close(context.Background())

	// pipe the importer straight into the exporter.
	records := make(chan *integration.Record, 256)
	toExporter := make(chan *integration.Record, 256)
	results := make(chan *integration.Result, 256)
	exportResults := make(chan *integration.Result, 256)

	go func() {
		if _, err := imp.Import(context.Background(), records, results); err != nil {
			t.Errorf("Import: %v", err)
		}
	}()

	go func() {
		defer close(toExporter)
		for record := range records {
			toExporter <- record
		}
	}()

	go func() {
		if err := exp.Export(context.Background(), nil, toExporter, exportResults); err != nil {
			t.Errorf("Export: %v", err)
		}
	}()

	for result := range exportResults {
		if result.Err != nil {
			t.Errorf("restore of %s failed: %v", result.Record.Pathname, result.Err)
		}
	}

	// everything the default selection covers must land byte for byte.
	for _, rel := range []string{
		"documents/Dune.azw3",
		"documents/Papers/Paper.pdf",
		"documents/My Clippings.txt",
		"documents/Dune.sdr/Dune.mbp1",
		"documents/Dune.sdr/assets/cover.jpg",
	} {
		want, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read source %s: %v", rel, err)
		}
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("restored file missing: %s: %v", rel, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s: restored %q, want %q", rel, got, want)
		}
	}

	// and nothing excluded should have been written.
	for _, rel := range []string{
		"system/thumbnails/thumb_Dune.jpg",
		"documents/half-download.azw3.tmp",
	} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s should not have been restored", rel)
		}
	}

	// no temporary files left behind.
	entries, err := os.ReadDir(filepath.Join(dst, kindle.DocumentsDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			t.Errorf("leftover temporary file: %s", entry.Name())
		}
	}
}
