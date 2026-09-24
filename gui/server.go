package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

//go:embed ui
var uiFiles embed.FS

const (
	attachmentHeader = "[Attached files]"
	uploadDir        = "uploads"
	maxUploadBytes   = 1 << 30
	maxTextPreview   = 2 << 20
)

func (app *App) handler() http.Handler {
	static, _ := fs.Sub(uiFiles, "ui")
	files := http.FileServerFS(static)
	mux := http.NewServeMux()
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self'; style-src 'self' 'unsafe-inline'; frame-src 'self'; frame-ancestors 'self'")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /api/state", app.handleState)
	mux.HandleFunc("PUT /api/config", app.handleConfig)
	mux.HandleFunc("GET /api/models", app.handleModels)
	mux.HandleFunc("GET /api/github", app.handleGitHub)
	mux.HandleFunc("POST /api/pick-folder", app.handlePickFolder)
	mux.HandleFunc("DELETE /api/sessions/{id}", app.handleDelete)
	mux.HandleFunc("GET /api/sessions/{id}/events", app.handleEvents)
	mux.HandleFunc("POST /api/sessions/{id}/messages", app.handleMessage)
	mux.HandleFunc("POST /api/sessions/{id}/stop", app.handleStop)
	mux.HandleFunc("POST /api/sessions/{id}/retry", app.handleRetry)
	mux.HandleFunc("POST /api/sessions/{id}/approve", app.handleApprove)
	mux.HandleFunc("GET /api/files", app.handleFiles)
	mux.HandleFunc("GET /api/raw", app.handleRaw)
	mux.HandleFunc("GET /api/text", app.handleText)
	mux.HandleFunc("POST /api/open", app.handleOpen)
	mux.HandleFunc("POST /api/upload", app.handleUpload)
	return app.guard(mux)
}

// guard admits only loopback Host headers (blocking DNS rebinding) and
// requests carrying the session cookie; state changes also need a custom
// header, which cross-origin pages cannot send without a CORS preflight.
func (app *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if token := r.URL.Query().Get("token"); token != "" && r.URL.Path == "/" {
			if subtle.ConstantTimeCompare([]byte(token), []byte(app.token)) == 1 {
				http.SetCookie(w, &http.Cookie{
					Name: "uag", Value: app.token, Path: "/", MaxAge: 365 * 24 * 3600,
					HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		cookie, err := r.Cookie("uag")
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(app.token)) != 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Unreal Agent</title><body style="font:15px system-ui;padding:3em">Open the link printed in the terminal where <code>unreal-agent-gui</code> is running.</body>`)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-UAG") != "1" {
			http.Error(w, "missing X-UAG header", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.MarshalWrite(w, value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeBody(r *http.Request, value any) error {
	return json.UnmarshalRead(http.MaxBytesReader(nil, r.Body, 4<<20), value)
}

func (app *App) handleState(w http.ResponseWriter, r *http.Request) {
	running := app.runningChats()
	type sessionView struct {
		SessionMeta
		Running bool `json:"running"`
	}
	sessions := []sessionView{}
	for _, meta := range app.sessionList() {
		sessions = append(sessions, sessionView{SessionMeta: meta, Running: running[meta.ID]})
	}
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, map[string]any{
		"config":    app.config().view(),
		"providers": providers,
		"thinking":  thinkingLevels,
		"sessions":  sessions,
		"data_dir":  app.dataDir,
		"home":      home,
	})
}

func (app *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	var update configUpdate
	if err := decodeBody(r, &update); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cfg, err := app.updateConfig(update)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if update.Permission != nil && cfg.Permission == permissionAuto {
		app.approveAll()
	}
	writeJSON(w, http.StatusOK, cfg.view())
}

func (app *App) handleModels(w http.ResponseWriter, r *http.Request) {
	p, ok := providerNamed(r.URL.Query().Get("provider"))
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown provider"))
		return
	}
	models, err := p.models(r.Context(), app.config())
	response := map[string]any{"models": models}
	if err != nil {
		response["error"] = err.Error() // the recommended picks still work
	}
	writeJSON(w, http.StatusOK, response)
}

var ghAccount = regexp.MustCompile(`Logged in to (\S+) account (\S+)`)

func (app *App) handleGitHub(w http.ResponseWriter, r *http.Request) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		connected := os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != ""
		writeJSON(w, http.StatusOK, map[string]any{"installed": false, "connected": connected})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, gh, "auth", "status").CombinedOutput()
	account := ""
	if match := ghAccount.FindSubmatch(output); match != nil {
		account = string(match[2])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installed": true, "connected": err == nil, "account": account, "output": string(output),
	})
}

func (app *App) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	var command *exec.Cmd
	switch {
	case goruntime.GOOS == "darwin":
		command = exec.CommandContext(r.Context(), "osascript", "-e", `POSIX path of (choose folder with prompt "Choose a workspace for new chats")`)
	case hasCommand("zenity"):
		command = exec.CommandContext(r.Context(), "zenity", "--file-selection", "--directory", "--title=Choose a workspace")
	case hasCommand("kdialog"):
		command = exec.CommandContext(r.Context(), "kdialog", "--getexistingdirectory")
	default:
		writeError(w, http.StatusNotImplemented, errors.New("no folder picker available"))
		return
	}
	output, err := command.Output()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"path": ""})
		return
	}
	folder := strings.TrimSpace(string(output))
	if folder != "/" {
		folder = strings.TrimRight(folder, "/")
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": folder})
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (app *App) sessionFromPath(w http.ResponseWriter, r *http.Request) (SessionMeta, bool) {
	meta, ok := app.session(session.ID(r.PathValue("id")))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown session"))
	}
	return meta, ok
}

func (app *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	meta, ok := app.sessionFromPath(w, r)
	if !ok {
		return
	}
	if err := app.deleteSession(meta.ID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (app *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	meta, ok := app.sessionFromPath(w, r)
	if !ok {
		return
	}
	current, err := app.chat(meta.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	after, _ := strconv.ParseUint(cmp.Or(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")), 10, 64)
	backlog, events, status := current.subscribe(after)
	defer current.unsubscribe(events)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	out := bufio.NewWriterSize(w, 64<<10)
	write := func(ev event) error {
		if ev.id != 0 {
			fmt.Fprintf(out, "id: %d\n", ev.id)
		}
		fmt.Fprintf(out, "event: %s\ndata: %s\n\n", ev.name, ev.data)
		return nil
	}
	flush := func() error {
		if err := out.Flush(); err != nil {
			return err
		}
		return controller.Flush()
	}
	fmt.Fprint(out, "retry: 1500\n\n")
	for _, item := range backlog {
		write(event{name: "item", id: item.seq, data: item.data})
	}
	write(status)
	if flush() != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			write(ev)
		case <-ping.C:
			fmt.Fprint(out, ": ping\n\n")
		}
		// Drain whatever else is queued before flushing.
		for drained := false; !drained; {
			select {
			case ev, open := <-events:
				if !open {
					flush()
					return
				}
				write(ev)
			default:
				drained = true
			}
		}
		if flush() != nil {
			return
		}
	}
}

func (app *App) handleMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text        string   `json:"text"`
		Attachments []string `json:"attachments"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	text := composeMessage(body.Text, body.Attachments)
	if text == "" {
		writeError(w, http.StatusBadRequest, errors.New("message is empty"))
		return
	}
	var meta SessionMeta
	if r.PathValue("id") == "new" {
		var err error
		if meta, err = app.createSession(session.ID(uuid.New().String())); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else if existing, ok := app.sessionFromPath(w, r); ok {
		meta = existing
	} else {
		return
	}
	current, err := app.chat(meta.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	app.touchSession(meta.ID, text)
	meta, _ = app.session(meta.ID)
	if err := current.send(text); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"session": meta, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": meta})
}

// composeMessage appends attachment paths in a block the UI renders as chips.
// External inputs are text-only, so the agent opens attachments with its tools.
func composeMessage(text string, attachments []string) string {
	text = strings.TrimSpace(text)
	var lines []string
	for _, attachment := range attachments {
		attachment = path.Clean(filepath.ToSlash(strings.TrimSpace(attachment)))
		if attachment == "." || strings.HasPrefix(attachment, "../") || path.IsAbs(attachment) {
			continue
		}
		line := "- " + attachment
		if isImage(attachment) {
			line += " (image: view it with ViewImage)"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return text
	}
	block := attachmentHeader + "\n" + strings.Join(lines, "\n")
	if text == "" {
		return block
	}
	return text + "\n\n" + block
}

func (app *App) handleApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"` // empty for all
		Approve bool   `json:"approve"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	app.withChat(w, r, func(c *chat) error { return c.decide(operation.ID(body.ID), body.Approve) })
}

func (app *App) handleStop(w http.ResponseWriter, r *http.Request) {
	app.withChat(w, r, (*chat).stop)
}

func (app *App) handleRetry(w http.ResponseWriter, r *http.Request) {
	app.withChat(w, r, (*chat).retry)
}

func (app *App) withChat(w http.ResponseWriter, r *http.Request, action func(*chat) error) {
	meta, ok := app.sessionFromPath(w, r)
	if !ok {
		return
	}
	current, err := app.chat(meta.ID)
	if err == nil {
		err = action(current)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// workspace resolves ?session= to that chat's workspace, or the default
// workspace for chats not created yet.
func (app *App) workspace(r *http.Request) (string, error) {
	if id := r.URL.Query().Get("session"); id != "" {
		meta, ok := app.session(session.ID(id))
		if !ok {
			return "", errors.New("unknown session")
		}
		return meta.Workspace, nil
	}
	workspace := app.config().Workspace
	return workspace, os.MkdirAll(workspace, 0o755)
}

// openRoot confines file access to the workspace; os.Root also rejects
// symlinks that escape it.
func (app *App) openRoot(w http.ResponseWriter, r *http.Request) (*os.Root, string, bool) {
	workspace, err := app.workspace(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, "", false
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, "", false
	}
	return root, workspace, true
}

func relativePath(value string) string {
	cleaned := path.Clean("/" + filepath.ToSlash(value))
	return strings.TrimPrefix(cleaned, "/")
}

func (app *App) handleFiles(w http.ResponseWriter, r *http.Request) {
	root, workspace, ok := app.openRoot(w, r)
	if !ok {
		return
	}
	defer root.Close()
	dir := relativePath(r.URL.Query().Get("dir"))
	name := cmp.Or(dir, ".")
	directory, err := root.Open(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	type entryView struct {
		Name     string    `json:"name"`
		Path     string    `json:"path"`
		Dir      bool      `json:"dir"`
		Size     int64     `json:"size"`
		Modified time.Time `json:"modified"`
	}
	showHidden := r.URL.Query().Get("hidden") == "1"
	views := []entryView{}
	for _, entry := range entries {
		if !showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		isDir := entry.IsDir()
		if entry.Type()&fs.ModeSymlink != 0 {
			if target, err := root.Stat(path.Join(name, entry.Name())); err == nil {
				isDir = target.IsDir()
			}
		}
		views = append(views, entryView{
			Name: entry.Name(), Path: path.Join(dir, entry.Name()), Dir: isDir,
			Size: info.Size(), Modified: info.ModTime(),
		})
	}
	slices.SortFunc(views, func(a, b entryView) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	writeJSON(w, http.StatusOK, map[string]any{"root": workspace, "dir": dir, "entries": views})
}

func (app *App) handleRaw(w http.ResponseWriter, r *http.Request) {
	root, _, ok := app.openRoot(w, r)
	if !ok {
		return
	}
	defer root.Close()
	name := relativePath(r.URL.Query().Get("path"))
	file, err := root.Open(cmp.Or(name, "."))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("not a file"))
		return
	}
	extension := strings.ToLower(path.Ext(name))
	if contentType := mime.TypeByExtension(extension); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else if textExtensions[extension] {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	// Agent-written files run in an opaque origin so they cannot reach this
	// API. Chrome's PDF viewer refuses to render under a sandbox, so PDFs are
	// exempt; they cannot script the page.
	if extension != ".pdf" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-popups allow-forms; frame-ancestors 'self'")
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
	}
	http.ServeContent(w, r, path.Base(name), info.ModTime(), file)
}

func (app *App) handleText(w http.ResponseWriter, r *http.Request) {
	root, workspace, ok := app.openRoot(w, r)
	if !ok {
		return
	}
	defer root.Close()
	name := relativePath(r.URL.Query().Get("path"))
	if _, err := root.Stat(cmp.Or(name, ".")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	text, err := documentText(filepath.Join(workspace, filepath.FromSlash(name)))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, paginate(text, 0, maxTextPreview))
}

func (app *App) handleOpen(w http.ResponseWriter, r *http.Request) {
	root, workspace, ok := app.openRoot(w, r)
	if !ok {
		return
	}
	defer root.Close()
	name := relativePath(r.URL.Query().Get("path"))
	if _, err := root.Stat(cmp.Or(name, ".")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	target := filepath.Join(workspace, filepath.FromSlash(name))
	var err error
	switch goruntime.GOOS {
	case "darwin":
		args := []string{target}
		if r.URL.Query().Get("reveal") == "1" {
			args = []string{"-R", target}
		}
		err = exec.Command("open", args...).Start()
	default:
		err = exec.Command("xdg-open", target).Start()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}._ -]+`)

func (app *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	root, _, ok := app.openRoot(w, r)
	if !ok {
		return
	}
	defer root.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	parts, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := root.Mkdir(uploadDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type uploaded struct {
		Path  string `json:"path"`
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		Image bool   `json:"image"`
	}
	saved := []uploaded{}
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if part.FileName() == "" {
			continue
		}
		name := strings.TrimSpace(unsafeName.ReplaceAllString(filepath.Base(part.FileName()), "_"))
		if name == "" || name == "." || name == ".." {
			name = "upload"
		}
		file, relative, err := createUnique(root, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		size, copyErr := io.Copy(file, part)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			root.Remove(relative)
			writeError(w, http.StatusBadRequest, err)
			return
		}
		saved = append(saved, uploaded{Path: relative, Name: path.Base(relative), Size: size, Image: isImage(relative)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": saved})
}

func createUnique(root *os.Root, name string) (*os.File, string, error) {
	extension := path.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := name
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, attempt, extension)
		}
		relative := path.Join(uploadDir, candidate)
		file, err := root.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, relative, err
	}
	return nil, "", errors.New("too many files with the same name")
}

var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true,
}

func isImage(name string) bool {
	return imageExtensions[strings.ToLower(path.Ext(name))]
}

var textExtensions = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".log": true, ".csv": true, ".tsv": true, ".go": true,
	".py": true, ".ts": true, ".tsx": true, ".jsx": true, ".rs": true, ".rb": true, ".java": true, ".c": true,
	".h": true, ".cpp": true, ".sh": true, ".zsh": true, ".yaml": true, ".yml": true, ".toml": true,
	".ini": true, ".env": true, ".sql": true, ".swift": true, ".kt": true, ".jsonl": true, ".ipynb": true,
}
