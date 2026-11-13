package kindle

import "testing"

func TestSelectionSkipDefault(t *testing.T) {
	sel := DefaultSelection()

	cases := []struct {
		path string
		dir  bool
		skip bool
	}{
		// books are the point of the exercise.
		{"documents", true, false},
		{"documents/Dune.azw3", false, false},
		{"documents/Some Paper.pdf", false, false},
		{"documents/Comic.cbz", false, false},

		// annotations travel with the books.
		{"documents/Dune.sdr", true, false},
		{"documents/Dune.sdr/Dune.mbp1", false, false},
		{"documents/My Clippings.txt", false, false},

		// device bookkeeping is not wanted by default.
		{"system", true, true},
		{"system/collections.json", false, true},

		// never worth copying, whatever the selection.
		{"system/thumbnails", true, true},
		{"system/thumbnails/thumb.jpg", false, true},
		{"system/search indexes", true, true},
		{".DS_Store", false, true},
		{"documents/.DS_Store", false, true},

		// debris the Kindle cannot open.
		{"documents/Dune.azw3.tmp", false, true},
		{"documents/leftover.mbp", false, true},
	}

	for _, c := range cases {
		if got := sel.Skip(c.path, c.dir); got != c.skip {
			t.Errorf("Skip(%q, dir=%v) = %v, want %v", c.path, c.dir, got, c.skip)
		}
	}
}

func TestSelectionDocumentsOnly(t *testing.T) {
	sel := Selection{Documents: true}

	// with annotations off the sidecars go, the books stay.
	if !sel.Skip("documents/Dune.sdr", true) {
		t.Error("sidecar directory should be skipped when annotations are off")
	}
	if !sel.Skip("documents/Dune.sdr/state", false) {
		t.Error("sidecar contents should be skipped when annotations are off")
	}
	if !sel.Skip("documents/My Clippings.txt", false) {
		t.Error("clippings should be skipped when annotations are off")
	}
	if sel.Skip("documents/Dune.azw3", false) {
		t.Error("book should be kept when documents are on")
	}
}

func TestSelectionAnnotationsOnly(t *testing.T) {
	sel := Selection{Annotations: true}

	// the books go, but documents/ still has to be walked for the sidecars.
	if sel.Skip("documents", true) {
		t.Error("documents/ must be walked to reach sidecars")
	}
	if sel.Skip("documents/Dune.sdr", true) {
		t.Error("sidecar should be kept when annotations are on")
	}
	if sel.Skip("documents/My Clippings.txt", false) {
		t.Error("clippings should be kept when annotations are on")
	}
	if !sel.Skip("documents/Dune.azw3", false) {
		t.Error("book should be skipped when documents are off")
	}
}

func TestSelectionSystem(t *testing.T) {
	sel := Selection{System: true}

	if sel.Skip("system", true) {
		t.Error("system/ should be kept when system is on")
	}
	if sel.Skip("system/collections.json", false) {
		t.Error("collections should be kept when system is on")
	}
	// caches stay excluded: large and regenerable.
	if !sel.Skip("system/thumbnails/x.jpg", false) {
		t.Error("thumbnails should be skipped even when system is on")
	}
}

func TestSelectionEverything(t *testing.T) {
	sel := Selection{Everything: true}

	for _, p := range []string{
		"system/thumbnails/x.jpg",
		"system/search indexes/idx",
		"documents/Dune.azw3.tmp",
		"documents/leftover.mbp",
		"whatever/else",
	} {
		if sel.Skip(p, false) {
			t.Errorf("Skip(%q) should be false with everything=true", p)
		}
	}

	// host-OS droppings are noise even in a verbatim copy.
	if !sel.Skip("documents/.DS_Store", false) {
		t.Error(".DS_Store should be skipped even with everything=true")
	}
}

// notebooks exist nowhere else, so they follow the annotations flag.
func TestSelectionNotebooks(t *testing.T) {
	sel := DefaultSelection()

	for _, p := range []string{
		".notebooks",
		".notebooks/B001KW08ZY!!EBOK!!notebook",
		".notebooks/B001KW08ZY!!EBOK!!notebook/nbk",
	} {
		if sel.Skip(p, false) {
			t.Errorf("Skip(%q) = true, notebooks should be backed up by default", p)
		}
	}

	off := Selection{Documents: true}
	if !off.Skip(".notebooks/x/nbk", false) {
		t.Error("notebooks should be skipped when annotations are off")
	}
}

// content the user side-loaded follows the documents flag.
func TestSelectionUserContentDirs(t *testing.T) {
	sel := DefaultSelection()

	for _, p := range []string{
		"screenshots/screenshot_01.png",
		"fonts/MyFont.ttf",
		"audible/book.aax",
		"voice/memo.wav",
	} {
		if sel.Skip(p, false) {
			t.Errorf("Skip(%q) = true, user content should be backed up by default", p)
		}
	}

	off := Selection{Annotations: true}
	for _, p := range []string{"screenshots/x.png", "fonts/x.ttf"} {
		if !off.Skip(p, false) {
			t.Errorf("Skip(%q) = false, should be skipped when documents are off", p)
		}
	}
}

// exclusions must match either spelling.
func TestSelectionCaseInsensitiveExclusions(t *testing.T) {
	sel := Selection{System: true}

	for _, p := range []string{
		"system/Search Indexes/idx",
		"system/search indexes/idx",
		"system/thumbnails/t.jpg",
		"system/grok_thumbnails/t.jpg",
		"system/CloudIndices/x",
		"system/readingstreams/x",
		"documents/dictionaries/dict.azw",
	} {
		if !sel.Skip(p, false) {
			t.Errorf("Skip(%q) = false, want true (regenerable or shipped data)", p)
		}
	}

	// real content in system/ still comes through.
	if sel.Skip("system/version.txt", false) {
		t.Error("system/version.txt should be kept when system is on")
	}
}

func TestIsBook(t *testing.T) {
	for _, name := range []string{
		"Dune.azw3", "Dune.AZW3", "book.mobi", "paper.pdf", "novel.epub",
		"notes.txt", "comic.cbz", "old.prc", "new.kfx",
	} {
		if !IsBook(name) {
			t.Errorf("IsBook(%q) = false, want true", name)
		}
	}

	for _, name := range []string{
		"Dune.azw3.tmp", "cover.jpg", "state.mbp", "index.idx", "noext",
	} {
		if IsBook(name) {
			t.Errorf("IsBook(%q) = true, want false", name)
		}
	}
}
