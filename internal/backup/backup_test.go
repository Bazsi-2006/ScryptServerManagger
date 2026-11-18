package backup

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setup(t *testing.T) (dir, outside string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "backups")
	outside = filepath.Join(base, "outside")
	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(dir, "world-1.tar.gz"), []byte("backup one"), 0o600)
	os.WriteFile(filepath.Join(dir, "world-2.tar.gz"), []byte("backup two!!"), 0o600)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "subdir", "nested.tar"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP SECRET"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "world-1.tar.gz"), old, old)
	return dir, outside
}

func TestListOnlyRegularFilesNewestFirst(t *testing.T) {
	dir, outside := setup(t)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "escape-abs"))
	os.Symlink("../outside/secret.txt", filepath.Join(dir, "escape-rel"))
	os.Symlink("world-1.tar.gz", filepath.Join(dir, "latest"))
	files, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	want := map[string]bool{"world-2.tar.gz": true, "world-1.tar.gz": true, "latest": true}
	if len(names) != len(want) {
		t.Fatalf("listed %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("%q must not be listed (hidden, directory or escaping symlink)", n)
		}
	}
	if names[0] != "world-2.tar.gz" || files[0].Size != 12 {
		t.Errorf("newest first with size: %+v", files[0])
	}
}

func TestOpenRejectsTraversalAndEscapes(t *testing.T) {
	dir, outside := setup(t)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "escape-abs"))
	os.Symlink("../outside/secret.txt", filepath.Join(dir, "escape-rel"))
	os.Symlink(outside, filepath.Join(dir, "escape-dir"))
	invalid := []string{"", ".", "..", "../outside/secret.txt", "subdir/nested.tar", "/etc/passwd", `a\b`, ".hidden", "nul\x00", "..\\x", "a\nb"}
	for _, name := range invalid {
		if _, _, err := Open(dir, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Open(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	for _, name := range []string{"escape-abs", "escape-rel", "escape-dir", "subdir", "missing.tar"} {
		f, _, err := Open(dir, name)
		if err == nil {
			data, _ := io.ReadAll(f)
			f.Close()
			t.Errorf("Open(%q) succeeded and returned %q", name, data)
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", name, err)
		}
	}
	f, info, err := Open(dir, "world-1.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "backup one" || info.Size() != 10 {
		t.Fatalf("content %q", data)
	}
}

func TestDeleteStaysInsideDirectory(t *testing.T) {
	dir, outside := setup(t)
	secret := filepath.Join(outside, "secret.txt")
	os.Symlink(secret, filepath.Join(dir, "link-out"))
	if err := Delete(dir, "../outside/secret.txt"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("traversal delete: %v", err)
	}
	if err := Delete(dir, "subdir"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("directories must not be deletable: %v", err)
	}
	// Deleting a symlink removes the link, never its target.
	if err := Delete(dir, "link-out"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("the symlink target outside the directory was deleted")
	}
	if err := Delete(dir, "world-1.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "world-1.tar.gz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestMissingDirectoryIsUnavailable(t *testing.T) {
	if _, err := List(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}
