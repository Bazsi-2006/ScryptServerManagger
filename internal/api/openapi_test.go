package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every registered API route must be described in the OpenAPI document, so
// the documentation cannot silently fall behind the code.
func TestOpenAPIDocumentsEveryRoute(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(OpenAPISpec)
	routes := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) "\+v\+"([^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) < 40 {
		t.Fatalf("only %d routes found; did the registration style change?", len(routes))
	}
	for _, r := range routes {
		method, path := strings.ToLower(r[1]), r[2]
		if path == "/meta" || path == "/openapi.yaml" {
			continue
		}
		idx := strings.Index(spec, "\n  "+path+":\n")
		if idx < 0 {
			t.Errorf("route %s %s is missing from openapi.yaml", r[1], path)
			continue
		}
		// The method must appear inside this path item (before the next one).
		rest := spec[idx+1:]
		if end := regexp.MustCompile(`\n  /`).FindStringIndex(rest[2:]); end != nil {
			rest = rest[:end[0]+2]
		}
		if !strings.Contains(rest, "\n    "+method+":") {
			t.Errorf("method %s of %s is missing from openapi.yaml", r[1], path)
		}
	}
}
