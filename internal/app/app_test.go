package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/testutil"
)

func TestEmbeddedFrontendIsServed(t *testing.T) {
	a, err := Build(testutil.Config(t), slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler)
	defer func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		a.Close(ctx)
	}()
	get := func(path string, hdr ...string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, string(body)
	}

	res, body := get("/")
	if res.StatusCode != 200 || !strings.Contains(body, `<script type="module" src="/js/main.js">`) {
		t.Fatalf("index: %d", res.StatusCode)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "onclick=") || strings.Contains(body, " style=") {
		t.Error("the page must not contain inline scripts or styles (CSP)")
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP on the frontend: %q", csp)
	}
	for path, ctype := range map[string]string{"/js/main.js": "text/javascript", "/css/app.css": "text/css", "/favicon.svg": "image/svg+xml", "/js/pages/server.js": "text/javascript"} {
		res, _ := get(path)
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	// Conditional requests avoid re-downloading unchanged assets.
	res, _ = get("/js/main.js")
	etag := res.Header.Get("ETag")
	if res2, _ := get("/js/main.js", "If-None-Match", etag); etag == "" || res2.StatusCode != http.StatusNotModified {
		t.Errorf("ETag revalidation: %q -> %d", etag, res2.StatusCode)
	}
	// Unknown non-file paths get the application shell; missing assets and
	// API routes do not.
	if res, body := get("/some/client/route"); res.StatusCode != 200 || !strings.Contains(body, "main.js") {
		t.Errorf("SPA fallback: %d", res.StatusCode)
	}
	if res, _ := get("/js/missing.js"); res.StatusCode != 404 {
		t.Errorf("missing asset: %d", res.StatusCode)
	}
	if res, body := get("/api/v1/does-not-exist"); res.StatusCode != 404 || !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("unknown API route should be a JSON 404: %d %s", res.StatusCode, body)
	}
	if res, _ := get("/../../etc/passwd"); res.StatusCode == 200 && !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Error("path traversal out of the embedded assets")
	}
	if res, body := get("/api/v1/openapi.yaml"); res.StatusCode != 200 || !strings.HasPrefix(body, "openapi:") {
		t.Errorf("OpenAPI document: %d", res.StatusCode)
	}
	if res, body := get("/api/v1/health"); res.StatusCode != 200 || !strings.Contains(body, "ok") {
		t.Errorf("health: %d", res.StatusCode)
	}
}

func TestValidUsername(t *testing.T) {
	for _, ok := range []string{"admin", "a.b-c_d", "user123", "Abc"} {
		if !ValidUsername(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "ab", "-lead", "has space", "a/b", "x@y", strings.Repeat("a", 33), "nul\x00"} {
		if ValidUsername(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
