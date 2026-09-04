package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// The UI is one program served as many files, and a browser keeps files. The
// one strategy against a half-upgraded UI is that every file of it is
// revalidated on every load: no version parameters, no bare-URL imports that
// a parameter could not reach anyway.

func TestTheUIIsRevalidatedOnEveryLoad(t *testing.T) {
	e := echo.New()
	registerUI(e, filepath.Join("..", "..", "web"))

	for _, path := range []string{"/", "/index.html", "/login.html", "/js/app.js",
		"/js/core/utils.js", "/js/modules/deliveries.js", "/css/styles.css"} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s is served with Cache-Control %q, want no-cache", path, got)
		}
		// Revalidation is cheap: a file that has not changed answers 304.
		modified := rec.Header().Get("Last-Modified")
		if modified == "" {
			t.Errorf("%s carries no Last-Modified, so nothing can revalidate it", path)
			continue
		}
		again := httptest.NewRequest(http.MethodGet, path, nil)
		again.Header.Set("If-Modified-Since", modified)
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, again)
		if rec.Code != http.StatusNotModified {
			t.Errorf("%s unchanged answered %d, want 304", path, rec.Code)
		}
	}
}

// TestTheUINamesItsFilesWithoutVersions: no version parameter anywhere - not
// on the script tags, which no-cache makes redundant, and not on the module
// imports, where one would split a module into two instances with two
// states.
func TestTheUINamesItsFilesWithoutVersions(t *testing.T) {
	web := filepath.Join("..", "..", "web")
	versioned := regexp.MustCompile(`(src|href)="[^"]*\?v=`)
	queryImport := regexp.MustCompile(`(from|import\()\s*['"][^'"]*\?[^'"]*['"]`)
	err := filepath.Walk(web, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(path, ".html"):
			if m := versioned.Find(body); m != nil {
				t.Errorf("%s names a file with a version parameter: %s", path, m)
			}
		case strings.HasSuffix(path, ".js"):
			if m := queryImport.Find(body); m != nil {
				t.Errorf("%s imports a module with a query string: %s", path, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
