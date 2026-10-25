package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestHandlerServesIndexAndVendor(t *testing.T) {
	w := get("/")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), `type="module" src="/js/app.js"`) {
		t.Fatalf("/: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index cache %q", w.Header().Get("Cache-Control"))
	}
	if w := get("/index.html"); w.Code != 200 {
		t.Errorf("/index.html: %d", w.Code)
	}
	w = get("/vendor/bootstrap/bootstrap.min.css")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") || !strings.Contains(w.Header().Get("Cache-Control"), "max-age=86400") {
		t.Errorf("css: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
	}
	if w := get("/js/app.js"); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("app.js: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get("/vendor/fonts/ibm-plex-sans-latin-400-normal.woff2"); w.Code != 200 {
		t.Errorf("font: %d", w.Code)
	}
	for _, p := range []string{"/nope.js", "/js/", "/../go.mod", "/static/index.html"} {
		if w := get(p); w.Code == 200 && p != "/js/" {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := get("/nope.js"); w.Code != http.StatusNotFound {
		t.Errorf("/nope.js: %d", w.Code)
	}
}

var (
	scriptTag   = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	handlerAttr = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	styleAttr   = regexp.MustCompile(`(?i)\sstyle\s*=`)
	styleTag    = regexp.MustCompile(`(?is)<style[\s>]`)
)

// The page runs under "default-src 'self'": anything inline would be blocked, so none may exist.
func TestNoInlineScripts(t *testing.T) {
	n := 0
	err := fs.WalkDir(content, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		n++
		b, err := content.ReadFile(path)
		if err != nil {
			return err
		}
		s := string(b)
		for _, m := range scriptTag.FindAllStringSubmatch(s, -1) {
			if strings.TrimSpace(m[2]) != "" {
				t.Errorf("%s: inline script content", path)
			}
		}
		for name, re := range map[string]*regexp.Regexp{"on* attribute": handlerAttr, "style attribute": styleAttr, "<style> block": styleTag} {
			if re.MatchString(s) {
				t.Errorf("%s: has %s", path, name)
			}
		}
		return nil
	})
	if err != nil || n < 2 {
		t.Fatalf("walked %d html files: %v", n, err)
	}
}

// The JS must not build inline style or handler attributes either (CSP applies to them at run time).
func TestNoInlineAttributesInJS(t *testing.T) {
	bad := regexp.MustCompile(`setAttribute\(\s*["'](style|on[a-z]+)["']|\.innerHTML\s*=|\sstyle:\s*"|insertAdjacentHTML`)
	err := fs.WalkDir(content, "static/js", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		b, err := content.ReadFile(path)
		if err != nil {
			return err
		}
		if m := bad.Find(b); m != nil {
			t.Errorf("%s: %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// go:embed silently skips names starting with "_" or "."; a page module named that way would 404 in production.
func TestNoHiddenFilesAreNeeded(t *testing.T) {
	err := fs.WalkDir(content, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".js") {
			b, _ := content.ReadFile(path)
			for _, m := range regexp.MustCompile(`from "(\./[^"]+|\.\./[^"]+)"`).FindAllStringSubmatch(string(b), -1) {
				base := m[1][strings.LastIndex(m[1], "/")+1:]
				if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
					t.Errorf("%s imports %s, which go:embed does not include", path, m[1])
				}
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
