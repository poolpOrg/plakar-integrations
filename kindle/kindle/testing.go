package kindle

import (
	"os"
	"path/filepath"
	"testing"
)

// NewTestVolume builds a synthetic Kindle tree.  it lives here rather than in
// a _test.go file so the connector tests can share it.
func NewTestVolume(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	files := map[string]string{
		"documents/Dune.azw3":                  "dune contents",
		"documents/Papers/Paper.pdf":           "%PDF-1.4 fake",
		"documents/My Clippings.txt":           "clipping one\n==========\n",
		"documents/Dune.sdr/Dune.mbp1":         "reading position",
		"documents/Dune.sdr/assets/cover.jpg":  "jpegdata",
		"documents/half-download.azw3.tmp":     "incomplete",
		"system/collections.json":              `{"collections":[]}`,
		"system/thumbnails/thumb_Dune.jpg":     "thumbnail",
		"system/search indexes/Dune.azw3.stmp": "index",
	}

	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", abs, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", abs, err)
		}
	}

	return root
}
