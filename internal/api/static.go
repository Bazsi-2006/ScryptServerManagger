package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

func contextTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

type staticFile struct {
	data  []byte
	etag  string
	ctype string
}

// staticFiles serves the embedded frontend from memory with strong ETags.
type staticFiles struct {
	files map[string]staticFile
}

func loadStatic(fsys fs.FS) (*staticFiles, error) {
	st := &staticFiles{files: map[string]staticFile{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		ctype := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".js":
			ctype = "text/javascript; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		case ".html":
			ctype = "text/html; charset=utf-8"
		case ".svg":
			ctype = "image/svg+xml"
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		st.files["/"+p] = staticFile{data: data, etag: `"` + hex.EncodeToString(sum[:12]) + `"`, ctype: ctype}
		return nil
	})
	return st, err
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.fail(w, r, errf(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.fail(w, r, errNotFound)
		return
	}
	if s.static == nil {
		http.NotFound(w, r)
		return
	}
	p := path.Clean("/" + r.URL.Path)
	f, ok := s.static.files[p]
	if !ok {
		if path.Ext(p) != "" {
			http.NotFound(w, r)
			return
		}
		// Single-page application: unknown paths get the shell.
		f = s.static.files["/index.html"]
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	// Revalidate on every load; unchanged files cost only a 304.
	h.Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(f.data)
	}
}
