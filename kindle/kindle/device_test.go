package kindle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsKindleVolume(t *testing.T) {
	root := NewTestVolume(t)
	if !IsKindleVolume(root) {
		t.Error("synthetic volume should be recognised as a Kindle")
	}

	if IsKindleVolume(t.TempDir()) {
		t.Error("empty directory should not be recognised as a Kindle")
	}

	// A documents *file* rather than a directory must not count.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, DocumentsDir), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsKindleVolume(other) {
		t.Error("documents as a regular file should not be recognised as a Kindle")
	}
}

func TestOpenExplicitPath(t *testing.T) {
	root := NewTestVolume(t)

	for _, location := range []string{root, "kindle://" + root, "kindle://" + root + "/"} {
		v, err := Open(location)
		if err != nil {
			t.Fatalf("Open(%q): %v", location, err)
		}
		if v.Root != root {
			t.Errorf("Open(%q) root = %q, want %q", location, v.Root, root)
		}
		if err := v.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", location, err)
		}
	}
}

func TestOpenRejectsBadPaths(t *testing.T) {
	if _, err := Open("kindle://" + filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("Open on a missing directory should fail")
	}

	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Error("Open on a regular file should fail")
	}
}

func TestValidateRejectsNonKindle(t *testing.T) {
	v := &Volume{Root: t.TempDir()}
	if err := v.Validate(); err == nil {
		t.Error("Validate should reject a directory without documents/")
	}
}

// Open must see through the storage directory an MTP mount exposes.
func TestOpenResolvesMTPStorageDir(t *testing.T) {
	mount := t.TempDir()
	inner := filepath.Join(mount, "Internal Storage")
	if err := os.MkdirAll(filepath.Join(inner, DocumentsDir), 0o755); err != nil {
		t.Fatal(err)
	}

	v, err := Open(mount)
	if err != nil {
		t.Fatalf("Open(%q): %v", mount, err)
	}
	if v.Root != inner {
		t.Errorf("root = %q, want %q", v.Root, inner)
	}
}

// a localised storage name must still be found.
func TestOpenResolvesUnknownStorageName(t *testing.T) {
	mount := t.TempDir()
	inner := filepath.Join(mount, "Stockage interne")
	if err := os.MkdirAll(filepath.Join(inner, DocumentsDir), 0o755); err != nil {
		t.Fatal(err)
	}

	v, err := Open(mount)
	if err != nil {
		t.Fatalf("Open(%q): %v", mount, err)
	}
	if v.Root != inner {
		t.Errorf("root = %q, want %q", v.Root, inner)
	}
}

func TestExtractSerial(t *testing.T) {
	cases := map[string]string{
		`{"serial":"G000AB01234567","x":1}`: "G000AB01234567",
		`{"SerialNumber":"B0171234"}`:       "B0171234",
		`{"other":"value"}`:                 "",
		``:                                  "",
	}

	for input, want := range cases {
		if got := extractSerial(input); got != want {
			t.Errorf("extractSerial(%q) = %q, want %q", input, got, want)
		}
	}
}

// MTP never exposes system/, so the serial comes from the mount point.
func TestSerialFromMTPPath(t *testing.T) {
	cases := map[string]string{
		"/run/user/1000/gvfs/mtp:host=Amazon_Kindle_Scribe_Colorsoft_G094PH08620701GQ":                  "G094PH08620701GQ",
		"/run/user/1000/gvfs/mtp:host=Amazon_Kindle_Scribe_G0923M10409300RQ":                            "G0923M10409300RQ",
		"/run/user/1000/gvfs/mtp:host=Amazon_Kindle_Scribe_Colorsoft_G094PH08620701GQ/Internal Storage": "G094PH08620701GQ",
		"/run/user/1000/gvfs/mtp:host=Amazon_Kindle_G0923M10409300RQ/Internal Storage/documents":        "G0923M10409300RQ",
		"/mnt/kindle":                          "",
		"/run/user/1000/gvfs/mtp:host=Amazon":  "", // no serial field
		"/run/user/1000/gvfs/mtp:host=A_Short": "", // too short to be a serial
	}

	for path, want := range cases {
		if got := serialFromPath(path); got != want {
			t.Errorf("serialFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// the path wins, but mass storage still reads the serial off the disk.
func TestSerialPrefersMTPPathOverFiles(t *testing.T) {
	v := &Volume{Root: "/run/user/1000/gvfs/mtp:host=Amazon_Kindle_Scribe_G0923M10409300RQ"}
	if got := v.Serial(); got != "G0923M10409300RQ" {
		t.Errorf("Serial() = %q, want the serial from the mount path", got)
	}
}

func TestSerialAbsentIsEmpty(t *testing.T) {
	v := &Volume{Root: NewTestVolume(t)}
	if s := v.Serial(); s != "" {
		t.Errorf("Serial() = %q, want empty for a device that exposes none", s)
	}
}

func TestSerialFromDeviceInfo(t *testing.T) {
	root := NewTestVolume(t)
	info := filepath.Join(root, SystemDir, ".device_info")
	if err := os.WriteFile(info, []byte(`{"serial":"G000AB01234567"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	v := &Volume{Root: root}
	if got := v.Serial(); got != "G000AB01234567" {
		t.Errorf("Serial() = %q, want G000AB01234567", got)
	}
}

func TestDocuments(t *testing.T) {
	v := &Volume{Root: "/mnt/kindle"}
	want := filepath.Join("/mnt/kindle", DocumentsDir)
	if got := v.Documents(); got != want {
		t.Errorf("Documents() = %q, want %q", got, want)
	}
}
