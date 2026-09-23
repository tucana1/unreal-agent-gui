package main

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Upstream keeps its provider table in an internal package, so this test
// catches providers added upstream after a sync.
func TestProvidersMatchUpstream(t *testing.T) {
	source, err := os.ReadFile("../cmd/internal/agentrunner/providers.go")
	if err != nil {
		t.Skipf("upstream providers not found: %v", err)
	}
	matches := regexp.MustCompile(`Name:\s+"([^"]+)"`).FindAllSubmatch(source, -1)
	if len(matches) == 0 {
		t.Fatal("found no providers upstream; update this test's pattern")
	}
	for _, match := range matches {
		if _, ok := providerNamed(string(match[1])); !ok {
			t.Errorf("upstream provider %q is missing from providers in config.go", match[1])
		}
	}
}

func TestComposeMessage(t *testing.T) {
	got := composeMessage(" Summarize these ", []string{"uploads/a.png", "../escape.txt", "/etc/passwd", "uploads/b.pdf"})
	want := "Summarize these\n\n[Attached files]\n- uploads/a.png (image: view it with ViewImage)\n- uploads/b.pdf"
	if got != want {
		t.Fatalf("composeMessage = %q, want %q", got, want)
	}
	if got := title(got); got != "Summarize these" {
		t.Fatalf("title = %q", got)
	}
	if got := title(composeMessage("", []string{"uploads/a.pdf"})); got != "Attached files" {
		t.Fatalf("title of attachment-only message = %q", got)
	}
}

func TestPaginate(t *testing.T) {
	text := strings.Repeat("é", 10) // 20 bytes
	page := paginate(text, 0, 7)
	if !strings.HasPrefix(page, "ééé\n\n[Truncated") || !strings.Contains(page, "-offset 6") {
		t.Fatalf("page = %q", page)
	}
	if got := paginate(text, 6, 0); got != strings.Repeat("é", 7) {
		t.Fatalf("offset page = %q", got)
	}
}

func TestHTMLToText(t *testing.T) {
	page := `<html><head><title>T</title><script>var x = 1;</script></head><body>
		<nav><a href="/home">Home</a></nav>
		<main><h2>Results</h2><p>First   <b>bold</b> line with a <a href="/doc">link</a>.</p>
		<ul><li>one</li><li>two</li></ul><pre>  keep
  spacing</pre></main><footer>ignored</footer></body></html>`
	base, _ := url.Parse("https://example.com/a/b")
	title, text := htmlToText([]byte(page), base)
	if title != "T" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"## Results", "First bold line with a [link](https://example.com/doc).", "- one\n- two", "  keep\n  spacing"} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"var x", "Home", "ignored"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("text contains %q:\n%s", unwanted, text)
		}
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, contents := range files {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		entry.Write([]byte(contents))
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	file.Close()
}

func TestDocumentText(t *testing.T) {
	dir := t.TempDir()
	docx := filepath.Join(dir, "a.docx")
	writeZip(t, docx, map[string]string{"word/document.xml": `<w:document xmlns:w="w"><w:body>
		<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:t xml:space="preserve"> world</w:t></w:r></w:p>
		<w:tbl><w:tr><w:tc><w:p><w:r><w:t>A1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>B1</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
		</w:body></w:document>`})
	xlsx := filepath.Join(dir, "a.xlsx")
	writeZip(t, xlsx, map[string]string{
		"xl/sharedStrings.xml":     `<sst><si><t>Name</t></si><si><r><t>Sc</t></r><r><t>ore</t></r></si></sst>`,
		"xl/workbook.xml":          `<workbook><sheets><sheet name="Results"/></sheets></workbook>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row><row><c r="B2"><v>42</v></c></row></sheetData></worksheet>`,
	})
	for path, want := range map[string]string{
		docx: "Hello world\nA1 | B1 | \n",
		xlsx: "## Results\nName\t\tScore\n\t42\n\n",
	} {
		got, err := documentText(path)
		if err != nil || got != want {
			t.Errorf("documentText(%s) = %q, %v; want %q", filepath.Base(path), got, err, want)
		}
	}
	if _, err := documentText(filepath.Join(dir, "missing.pdf")); err == nil {
		t.Error("missing file did not fail")
	}
}

func newTestApp(t *testing.T) (*App, string) {
	t.Helper()
	app, err := newApp(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	app.cfg.Workspace = workspace
	return app, workspace
}

func request(app *App, method, target string, cookie bool, header bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:7777"
	if cookie {
		r.AddCookie(&http.Cookie{Name: "uag", Value: app.token})
	}
	if header {
		r.Header.Set("X-UAG", "1")
	}
	w := httptest.NewRecorder()
	app.handler().ServeHTTP(w, r)
	return w
}

func TestGuard(t *testing.T) {
	app, _ := newTestApp(t)
	if code := request(app, "GET", "/api/state", false, false).Code; code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d", code)
	}
	if code := request(app, "GET", "/api/state", true, false).Code; code != http.StatusOK {
		t.Errorf("cookie: %d", code)
	}
	if code := request(app, "PUT", "/api/config", true, false).Code; code != http.StatusForbidden {
		t.Errorf("state change without X-UAG: %d", code)
	}
	r := httptest.NewRequest("GET", "/api/state", nil)
	r.Host = "attacker.example:7777"
	r.AddCookie(&http.Cookie{Name: "uag", Value: app.token})
	w := httptest.NewRecorder()
	app.handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("rebinding host: %d", w.Code)
	}
	w = request(app, "GET", "/?token="+app.token, false, false)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Errorf("token exchange: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
}

func TestFilesStayInWorkspace(t *testing.T) {
	app, workspace := newTestApp(t)
	os.WriteFile(filepath.Join(workspace, "notes.md"), []byte("# hi"), 0o644)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("secret"), 0o644)
	os.Symlink(secret, filepath.Join(workspace, "link.txt"))

	w := request(app, "GET", "/api/raw?path=notes.md", true, false)
	if w.Code != http.StatusOK || w.Body.String() != "# hi" {
		t.Fatalf("notes.md: %d %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Error("workspace files must be served sandboxed")
	}
	for _, path := range []string{"../" + filepath.Base(filepath.Dir(secret)) + "/secret.txt", secret, "link.txt"} {
		w := request(app, "GET", "/api/raw?path="+url.QueryEscape(path), true, false)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "secret\"") || w.Body.String() == "secret" {
			t.Errorf("%s escaped the workspace: %d %q", path, w.Code, w.Body.String())
		}
	}
}
