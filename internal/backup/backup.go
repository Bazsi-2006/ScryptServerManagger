// Package backup gives controlled access to the files in a server's backup
// directory. All access goes through os.Root, which refuses path traversal
// and symbolic links that lead outside the directory.
package backup

import (
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalidName = errors.New("invalid backup file name")
	ErrNotFound    = errors.New("backup file not found")
	// ErrUnavailable: the backup directory itself cannot be opened.
	ErrUnavailable = errors.New("backup directory is not accessible")
)

// File describes one backup file.
type File struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modified_at"`
}

// ValidName reports whether name is a plain file name: a single path
// element, not hidden, without separators or control characters.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func openRoot(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrUnavailable
	}
	return root, nil
}

// List returns the regular files directly inside dir, newest first.
// Subdirectories, hidden files, special files and symlinks that point
// outside the directory are skipped.
func List(dir string) ([]File, error) {
	root, err := openRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	d, err := root.Open(".")
	if err != nil {
		return nil, ErrUnavailable
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, ErrUnavailable
	}
	out := []File{}
	for _, e := range entries {
		name := e.Name()
		if !ValidName(name) {
			continue
		}
		// Stat through the root: follows symlinks only while they stay
		// inside the directory.
		info, err := root.Stat(name)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, File{Name: name, Size: info.Size(), ModTime: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Open opens one backup file for reading. The caller must close it.
func Open(dir, name string) (*os.File, fs.FileInfo, error) {
	if !ValidName(name) {
		return nil, nil, ErrInvalidName
	}
	root, err := openRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	// Check the opened descriptor, not the path, so the answer cannot change
	// between check and use.
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}

// Delete removes one backup file.
func Delete(dir, name string) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	root, err := openRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return ErrNotFound
	}
	// Only regular files and symlinks (the link itself, never its target).
	if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
		return ErrNotFound
	}
	if err := root.Remove(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
