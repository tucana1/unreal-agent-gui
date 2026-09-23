package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

//go:embed skills
var builtinSkills embed.FS

type SessionMeta struct {
	ID        session.ID `json:"id"`
	Title     string     `json:"title"`
	Workspace string     `json:"workspace"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type App struct {
	ctx      context.Context
	dataDir  string
	token    string
	store    *localfile.Store
	skillDir string

	mu       sync.Mutex
	cfg      Config
	sessions map[session.ID]*SessionMeta
	envSet   []string // names this app exported from cfg.Env

	chatsMu sync.Mutex
	chats   map[session.ID]*chat
	runs    sync.WaitGroup // running coordinators
}

func newApp(ctx context.Context, dataDir string) (*App, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	app := &App{
		ctx:      ctx,
		dataDir:  dataDir,
		skillDir: filepath.Join(dataDir, "skills"),
		sessions: make(map[session.ID]*SessionMeta),
		chats:    make(map[session.ID]*chat),
	}
	var err error
	if app.cfg, err = loadConfig(app.path("config.json")); err != nil {
		return nil, err
	}
	if err := app.loadSessions(); err != nil {
		return nil, err
	}
	if app.token, err = loadToken(app.path("token")); err != nil {
		return nil, err
	}
	if err := installSkills(app.skillDir); err != nil {
		return nil, err
	}
	if err := prepareEnvironment(); err != nil {
		return nil, err
	}
	app.applyEnv(app.cfg.Env)
	if app.store, err = localfile.New(app.path("sessions")); err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	// The store's observer list is not safe to change while items are written,
	// so a single observer registered before any coordinator starts fans out.
	app.store.AddObserver(app.observe)
	return app, nil
}

// shutdown cancels every run and waits briefly for coordinators to cancel
// their tool calls, so shell commands do not outlive the app.
func (app *App) shutdown(cancel context.CancelFunc, timeout time.Duration) {
	cancel()
	done := make(chan struct{})
	go func() {
		app.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (app *App) path(name string) string {
	return filepath.Join(app.dataDir, name)
}

func (app *App) config() Config {
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.cfg
}

func (app *App) updateConfig(update configUpdate) (Config, error) {
	app.mu.Lock()
	defer app.mu.Unlock()
	next, err := app.cfg.apply(update)
	if err != nil {
		return app.cfg, err
	}
	if err := saveConfig(app.path("config.json"), next); err != nil {
		return app.cfg, fmt.Errorf("save config: %w", err)
	}
	if update.Env != nil {
		app.applyEnvLocked(next.Env)
	}
	app.cfg = next
	return next, nil
}

// applyEnv exports configured variables so the agent's Bash commands see them.
func (app *App) applyEnv(env map[string]string) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.applyEnvLocked(env)
}

func (app *App) applyEnvLocked(env map[string]string) {
	for _, name := range app.envSet {
		if _, kept := env[name]; !kept {
			os.Unsetenv(name)
		}
	}
	app.envSet = app.envSet[:0]
	for name, value := range env {
		os.Setenv(name, value)
		app.envSet = append(app.envSet, name)
	}
}

// prepareEnvironment points skills at this binary and keeps CLI tools from
// waiting on prompts or pagers no one can answer.
func prepareEnvironment() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	os.Setenv("UAG", executable)
	for name, value := range map[string]string{
		"GIT_TERMINAL_PROMPT":   "0",
		"GH_PROMPT_DISABLED":    "1",
		"GH_NO_UPDATE_NOTIFIER": "1",
		"PAGER":                 "cat",
		"GIT_PAGER":             "cat",
	} {
		if _, set := os.LookupEnv(name); !set {
			os.Setenv(name, value)
		}
	}
	return nil
}

func (app *App) loadSessions() error {
	encoded, err := os.ReadFile(app.path("sessions.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read sessions: %w", err)
	}
	var list []*SessionMeta
	if err := json.Unmarshal(encoded, &list); err != nil {
		return fmt.Errorf("parse sessions.json: %w", err)
	}
	for _, meta := range list {
		app.sessions[meta.ID] = meta
	}
	return nil
}

func (app *App) saveSessionsLocked() error {
	list := app.sessionListLocked()
	encoded, err := json.Marshal(list, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	return writeFileAtomic(app.path("sessions.json"), encoded, 0o600)
}

func (app *App) sessionListLocked() []SessionMeta {
	list := make([]SessionMeta, 0, len(app.sessions))
	for _, meta := range app.sessions {
		list = append(list, *meta)
	}
	slices.SortFunc(list, func(a, b SessionMeta) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return list
}

func (app *App) sessionList() []SessionMeta {
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.sessionListLocked()
}

func (app *App) session(id session.ID) (SessionMeta, bool) {
	app.mu.Lock()
	defer app.mu.Unlock()
	meta, ok := app.sessions[id]
	if !ok {
		return SessionMeta{}, false
	}
	return *meta, true
}

func (app *App) createSession(id session.ID) (SessionMeta, error) {
	cfg := app.config()
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return SessionMeta{}, fmt.Errorf("create workspace: %w", err)
	}
	if _, err := app.store.Create(app.ctx, id); err != nil {
		return SessionMeta{}, err
	}
	now := time.Now().UTC()
	meta := &SessionMeta{ID: id, Workspace: cfg.Workspace, CreatedAt: now, UpdatedAt: now}
	app.mu.Lock()
	defer app.mu.Unlock()
	app.sessions[id] = meta
	return *meta, app.saveSessionsLocked()
}

func (app *App) touchSession(id session.ID, text string) error {
	app.mu.Lock()
	defer app.mu.Unlock()
	meta, ok := app.sessions[id]
	if !ok {
		return nil
	}
	meta.UpdatedAt = time.Now().UTC()
	if meta.Title == "" {
		meta.Title = title(text)
	}
	return app.saveSessionsLocked()
}

func (app *App) deleteSession(id session.ID) error {
	app.chatsMu.Lock()
	if current, ok := app.chats[id]; ok {
		if current.isRunning() {
			app.chatsMu.Unlock()
			return errors.New("stop the chat before deleting it")
		}
		delete(app.chats, id)
	}
	app.chatsMu.Unlock()
	app.mu.Lock()
	delete(app.sessions, id)
	err := app.saveSessionsLocked()
	app.mu.Unlock()
	return errors.Join(
		err,
		os.Remove(filepath.Join(app.path("sessions"), string(id)+".session.jsonl")),
		os.RemoveAll(filepath.Join(app.path("operations"), string(id))),
	)
}

func (app *App) observe(id session.ID, item sessionstore.Item) {
	app.chatsMu.Lock()
	current := app.chats[id]
	app.chatsMu.Unlock()
	if current != nil {
		current.publish(item)
	}
}

// skills returns workspace skills from .harness/skills (the upstream runner's
// location) plus built-in skills whose names the workspace does not override.
func (app *App) skills(workspace string) []tool.Skill {
	skills, _ := tool.DiscoverSkills(filepath.Join(workspace, ".harness", "skills"))
	builtin, _ := tool.DiscoverSkills(app.skillDir)
	for _, skill := range builtin {
		if !slices.ContainsFunc(skills, func(s tool.Skill) bool { return s.Name == skill.Name }) {
			skills = append(skills, skill)
		}
	}
	return skills
}

// installSkills writes the embedded skills to disk, where SkillUse reads them.
func installSkills(directory string) error {
	return fs.WalkDir(builtinSkills, "skills", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(directory, strings.TrimPrefix(path, "skills"))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		contents, err := builtinSkills.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0o644)
	})
}

func loadToken(path string) (string, error) {
	if encoded, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(encoded))) >= 32 {
		return strings.TrimSpace(string(encoded)), nil
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random)
	return token, writeFileAtomic(path, []byte(token), 0o600)
}

func title(text string) string {
	text = strings.TrimSpace(text)
	if before, _, found := strings.Cut(text, attachmentHeader); found {
		text = cmp.Or(strings.TrimSpace(before), "Attached files")
	}
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 60 {
		text = strings.TrimSpace(string(runes[:60])) + "…"
	}
	return text
}
