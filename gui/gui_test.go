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

	"github.com/unreallabsai/unreal-agent/harness/operation"
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

func TestProviderAvailability(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("FIREWORKS_API_KEY", "")
	cfg := defaultConfig()
	if _, ok := cfg.selectedProvider(); ok {
		t.Fatal("a provider is available without any key")
	}
	cfg.APIKeys = map[string]string{"openrouter": "test-key"}
	p, ok := cfg.selectedProvider()
	if !ok || p.Name != "openrouter" {
		t.Fatalf("selected = %q, %v", p.Name, ok)
	}
	if got := p.model(cfg); got != p.Models[0].ID || !strings.HasSuffix(got, ":free") {
		t.Fatalf("default model = %q, want the free option", got)
	}
	cfg.Models = map[string]string{"openrouter": "some/other-model"}
	if got := p.model(cfg); got != "some/other-model" {
		t.Fatalf("chosen model = %q", got)
	}
	codex, _ := providerNamed("openai-codex")
	if codex.available(cfg) {
		t.Fatal("Codex login is available without opting in")
	}
	cfg.Enabled = map[string]bool{"openai-codex": true}
	if !codex.available(cfg) {
		t.Fatal("enabled Codex login is unavailable")
	}
	ollama, _ := providerNamed("ollama")
	cfg.Enabled["ollama"] = true
	if !ollama.available(cfg) || ollama.model(cfg) != "" {
		t.Fatal("enabled Ollama should be available with no model chosen yet")
	}
}

func TestModelCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[
			{"id":"b/chat","name":"B","context_length":131072,"pricing":{"prompt":"0.0000001","completion":"0.0000005"},"supported_parameters":["tools","reasoning"]},
			{"id":"a/free:free","name":"A","pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
			{"id":"c/no-tools","pricing":{"prompt":"0","completion":"0"},"supported_parameters":["temperature"]},
			{"id":"b/chat:batch","supported_parameters":["tools"]}
		]}`))
	}))
	defer server.Close()
	p := provider{Name: "test-catalog", Label: "Test", BaseURL: server.URL, Catalog: true, Models: []modelOption{{ID: "a/free:free"}}}
	models, err := p.models(t.Context(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "a/free:free" || models[1].ID != "b/chat" {
		t.Fatalf("models = %+v", models)
	}
	if !models[0].Recommends || models[0].Input != 0 || models[0].Reasoning {
		t.Errorf("free model = %+v", models[0])
	}
	if models[1].Input != 0.1 || models[1].Output != 0.5 || !models[1].Reasoning || models[1].Context != 131072 {
		t.Errorf("paid model = %+v", models[1])
	}
}

type fakeManager struct {
	added   []operation.Operation
	updates chan operation.Operation
}

func (m *fakeManager) Add(op operation.Operation) error    { m.added = append(m.added, op); return nil }
func (m *fakeManager) Cancel(operation.ID, string) error   { return nil }
func (m *fakeManager) Updates() <-chan operation.Operation { return m.updates }

func shellOperation(t *testing.T, id, command string) operation.Operation {
	t.Helper()
	spec, err := operation.NewShellSpec(operation.ShellInput{Command: command, Shell: "/bin/sh", Directory: "/tmp"}, t.TempDir(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{
		ID: operation.ID(id), Type: spec.Type, Version: spec.Version, Status: operation.StatusReady,
		State: spec.State, MaxOutputLength: spec.MaxOutputLength,
	}
}

func TestApprovalGate(t *testing.T) {
	next := &fakeManager{updates: make(chan operation.Operation)}
	ask := true
	changes := 0
	gate := newApprovalGate(t.Context(), next, func() bool { return ask }, func() { changes++ })

	gate.Add(shellOperation(t, "a", "rm -rf build"))
	gate.Add(shellOperation(t, "b", "ls"))
	gate.Add(operation.Operation{ID: "img", Type: operation.TypeViewImage, Status: operation.StatusReady})
	if len(next.added) != 1 || next.added[0].ID != "img" {
		t.Fatalf("only non-shell operations should pass through in ask mode: %v", next.added)
	}
	if list := gate.list(); len(list) != 2 || list[0].Command != "rm -rf build" {
		t.Fatalf("pending = %+v", list)
	}

	if err := gate.decide("b", true); err != nil || len(next.added) != 2 || next.added[1].ID != "b" {
		t.Fatalf("approve: %v, added %v", err, next.added)
	}
	if err := gate.decide("a", false); err != nil {
		t.Fatal(err)
	}
	declined := <-gate.Updates()
	state, err := operation.DecodeShellState(declined)
	if err != nil || declined.ID != "a" || declined.Status != operation.StatusFailed || !strings.Contains(state.TerminalError, "declined") {
		t.Fatalf("declined update = %+v, %+v, %v", declined, state, err)
	}

	gate.Add(shellOperation(t, "c", "sleep 1"))
	if err := gate.Cancel("c", "stopped"); err != nil {
		t.Fatal(err)
	}
	if canceled := <-gate.Updates(); canceled.ID != "c" || canceled.Status != operation.StatusCanceled {
		t.Fatalf("canceled update = %+v", canceled)
	}

	ask = false
	gate.Add(shellOperation(t, "d", "echo auto"))
	if last := next.added[len(next.added)-1]; last.ID != "d" || len(gate.list()) != 0 {
		t.Fatal("full auto should run commands without holding them")
	}
	if changes == 0 {
		t.Fatal("pending changes were not reported")
	}
}
