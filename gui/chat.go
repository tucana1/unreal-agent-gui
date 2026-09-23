package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/coordinator"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	"github.com/unreallabsai/unreal-agent/harness/tool/viewimage"
)

const toolHeartbeatInterval = 10 * time.Minute

type event struct {
	name string
	id   uint64
	data []byte
}

type storedItem struct {
	seq  uint64
	data []byte
}

// chat owns one session: its cached history, live subscribers, and at most one
// running coordinator. A coordinator runs until the agent goes idle; messages
// sent while it runs join the same run through its inbox.
type chat struct {
	app *App
	id  session.ID

	mu      sync.Mutex
	items   []storedItem
	subs    map[chan event]struct{}
	running bool
	stopped bool
	inbox   *inbox.Inbox
	ctx     context.Context
	cancel  context.CancelFunc
	pending []inbox.Input // submitted external inputs not yet persisted
	lastErr string
}

func (app *App) chat(id session.ID) (*chat, error) {
	app.chatsMu.Lock()
	defer app.chatsMu.Unlock()
	if current, ok := app.chats[id]; ok {
		return current, nil
	}
	current := &chat{app: app, id: id, subs: make(map[chan event]struct{})}
	if err := current.loadHistory(); err != nil {
		return nil, err
	}
	app.chats[id] = current
	return current, nil
}

func (app *App) runningChats() map[session.ID]bool {
	app.chatsMu.Lock()
	defer app.chatsMu.Unlock()
	running := make(map[session.ID]bool)
	for id, current := range app.chats {
		if current.isRunning() {
			running[id] = true
		}
	}
	return running
}

func (c *chat) loadHistory() error {
	after := sessionstore.BeforeFirst
	for {
		page, err := c.app.store.Items(c.app.ctx, c.id, after, 256)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load session %s: %w", c.id, err)
		}
		for _, item := range page.Items {
			encoded, err := json.Marshal(item)
			if err != nil {
				return fmt.Errorf("encode session item %d: %w", item.Sequence, err)
			}
			c.items = append(c.items, storedItem{seq: uint64(item.Sequence), data: encoded})
		}
		if !page.More || page.NextAfter <= after {
			return nil
		}
		after = page.NextAfter
	}
}

func (c *chat) isRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// publish runs synchronously inside the store's append, on the coordinator's
// goroutine, so it only takes c.mu and never blocks on subscribers.
func (c *chat) publish(item sessionstore.Item) {
	encoded, err := json.Marshal(item)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, storedItem{seq: uint64(item.Sequence), data: encoded})
	if input, ok := item.Data.(inbox.Input); ok {
		c.pending = slices.DeleteFunc(c.pending, func(p inbox.Input) bool { return p.ID == input.ID })
	}
	c.broadcastLocked(event{name: "item", id: uint64(item.Sequence), data: encoded})
}

func (c *chat) broadcastLocked(ev event) {
	for ch := range c.subs {
		select {
		case ch <- ev:
		default:
			// A stalled client reconnects and resumes from Last-Event-ID.
			close(ch)
			delete(c.subs, ch)
		}
	}
}

func (c *chat) statusLocked() event {
	encoded, _ := json.Marshal(map[string]any{"running": c.running, "error": c.lastErr})
	return event{name: "status", data: encoded}
}

func (c *chat) subscribe(after uint64) ([]storedItem, chan event, event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := sort.Search(len(c.items), func(i int) bool { return c.items[i].seq > after })
	ch := make(chan event, 512)
	c.subs[ch] = struct{}{}
	return slices.Clone(c.items[start:]), ch, c.statusLocked()
}

func (c *chat) unsubscribe(ch chan event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.subs[ch]; ok {
		delete(c.subs, ch)
		close(ch)
	}
}

func (c *chat) send(text string) error {
	payload, err := json.Marshal(text)
	if err != nil {
		return err
	}
	input := inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputExternal, Payload: payload}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, input)
	c.lastErr = ""
	if !c.running {
		if err := c.startLocked(); err != nil {
			// Nothing was persisted; the UI puts the message back in the composer.
			c.pending = slices.DeleteFunc(c.pending, func(p inbox.Input) bool { return p.ID == input.ID })
			return err
		}
		return nil
	}
	// If the coordinator is already winding down, finished restarts it with
	// the pending input.
	if err := c.inbox.Submit(c.ctx, input); err != nil && c.ctx.Err() == nil {
		return err
	}
	return nil
}

// retry restarts an idle chat, for example after fixing a missing API key.
func (c *chat) retry() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return nil
	}
	c.lastErr = ""
	return c.startLocked()
}

// stop asks the coordinator to cancel tool calls and exit; a second stop
// cancels the run outright.
func (c *chat) stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return nil
	}
	if c.stopped {
		c.cancel()
		return nil
	}
	c.stopped = true
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.StopHard, Reason: "Stopped by the user."})
	if err != nil {
		return err
	}
	return c.inbox.Submit(c.ctx, inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload})
}

func (c *chat) startLocked() error {
	ctx, cancel := context.WithCancel(c.app.ctx)
	run, err := c.app.prepareRun(ctx, c.id, slices.Clone(c.pending))
	if err != nil {
		cancel()
		return err
	}
	c.running, c.stopped = true, false
	c.inbox, c.ctx, c.cancel = run.inbox, ctx, cancel
	c.broadcastLocked(c.statusLocked())
	c.app.runs.Add(1)
	go func() {
		defer c.app.runs.Done()
		err := run.coordinator.Run(ctx)
		cancel()
		c.finished(errors.Join(err, run.close()))
	}()
	return nil
}

func (c *chat) finished(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running, c.inbox = false, nil
	if c.stopped {
		c.pending = nil
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		c.lastErr = err.Error()
	}
	if err == nil && len(c.pending) > 0 {
		restartErr := c.startLocked()
		if restartErr == nil {
			return
		}
		c.lastErr = restartErr.Error()
	}
	c.broadcastLocked(c.statusLocked())
}

type preparedRun struct {
	coordinator coordinator.Coordinator
	inbox       *inbox.Inbox
	close       func() error
}

func (app *App) prepareRun(ctx context.Context, id session.ID, inputs []inbox.Input) (preparedRun, error) {
	cfg := app.config()
	meta, ok := app.session(id)
	if !ok {
		return preparedRun{}, fmt.Errorf("unknown session %s", id)
	}
	p, ok := providerNamed(cfg.Provider)
	if !ok {
		return preparedRun{}, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
	model := p.model(cfg)
	if model == "" {
		return preparedRun{}, fmt.Errorf("choose a model for %s in the toolbar", p.Label)
	}
	if err := os.MkdirAll(meta.Workspace, 0o755); err != nil {
		return preparedRun{}, fmt.Errorf("create workspace: %w", err)
	}
	restored, err := app.store.Resume(ctx, id)
	if err != nil {
		return preparedRun{}, fmt.Errorf("resume session: %w", err)
	}
	operationDir := filepath.Join(app.path("operations"), string(id))
	if err := os.MkdirAll(operationDir, 0o700); err != nil {
		return preparedRun{}, fmt.Errorf("create operation directory: %w", err)
	}
	llmClient, err := p.connect(cfg)
	if err != nil {
		return preparedRun{}, err
	}
	fail := func(err error) (preparedRun, error) {
		return preparedRun{}, errors.Join(err, llmClient.Close())
	}

	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	registry := tool.NewRegistry(tool.StaticTranslators{
		Bash:      bash.New(bash.Config{Shell: shell, Directory: meta.Workspace, BaseDirectory: operationDir}),
		ViewImage: viewimage.New(viewimage.Config{Directory: meta.Workspace}),
	}, tool.BashName, tool.ViewImageName, tool.SkillUseName)
	for _, skill := range app.skills(meta.Workspace) {
		if _, err := registry.RegisterSkill(skill); err != nil {
			return fail(fmt.Errorf("register skill %q: %w", skill.Name, err))
		}
	}

	effort := llm.ReasoningEffort(cfg.Thinking)
	if !effort.Valid() {
		effort = llm.ReasoningEffortHigh
	}
	builder := contextbuilder.NewBuilder(registry.Skills()...)
	builder.SetModel(llm.Model{ID: model, ReasoningEffort: effort})
	hostedSearch := cfg.WebSearch && p.HostedSearch
	builder.SetSystemPrompt(systemPrompt(meta.Workspace, cfg.Instructions, hostedSearch))
	for _, definition := range registry.StaticDefinitions() {
		builder.AddTool(definition.Tool)
	}
	if hostedSearch {
		builder.AddTool(llm.Tool{Type: llm.ToolHosted, Name: "web_search"})
	}

	in, err := inbox.New(ctx, restored.ExternalInputIDs)
	if err != nil {
		return fail(fmt.Errorf("open inbox: %w", err))
	}
	settings, err := controlInput(inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: effort}})
	if err != nil {
		return fail(err)
	}
	stopWhenIdle, err := controlInput(inbox.ControlMessage{Mode: inbox.StopWhenIdle})
	if err != nil {
		return fail(err)
	}
	// Stop-when-idle goes last so the queued messages keep the run busy.
	for _, input := range slices.Concat([]inbox.Input{settings}, inputs, []inbox.Input{stopWhenIdle}) {
		if err := in.Submit(ctx, input); err != nil {
			return fail(fmt.Errorf("submit input: %w", err))
		}
	}
	return preparedRun{
		coordinator: coordinator.New(coordinator.Dependencies{
			ToolHeartbeatInterval: toolHeartbeatInterval,
			SessionID:             id,
			Inbox:                 in,
			Restored:              restored,
			Sessions:              app.store,
			ContextBuilder:        builder,
			LLM:                   llmClient,
			Tools:                 registry,
			Operations:            operation.NewLocalOperationManager(ctx),
		}),
		inbox: in,
		close: llmClient.Close,
	}, nil
}

func controlInput(message inbox.ControlMessage) (inbox.Input, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return inbox.Input{}, fmt.Errorf("encode control message: %w", err)
	}
	return inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload}, nil
}

func systemPrompt(workspace, instructions string, hostedSearch bool) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, `You are a research and engineering agent working for the user through Unreal Agent GUI. You run directly on the user's computer, not in a sandbox.

## Environment
- Workspace: %s. Bash starts here. Save deliverables here; the user previews workspace files in the GUI's Files panel.
- Files the user attaches are saved under uploads/ in the workspace.
- Host: %s/%s. Today is %s.

## Guidelines
- Load the matching skill before specialized work: web-research for anything current, factual, or cited; documents for PDFs, Office files, and reports; github for GitHub.
`, workspace, goruntime.GOOS, goruntime.GOARCH, time.Now().Format("Monday, 2 January 2006"))
	if hostedSearch {
		prompt.WriteString("- You also have a native web_search tool. Use it for discovery, and the web-research skill's fetch command to read full pages.\n")
	}
	prompt.WriteString(`- Look at attached or generated images with ViewImage.
- Cite web sources as markdown links. Link workspace files by relative path, like [report.md](report.md), so the user can click to preview them.
- Ask before destructive or outward-facing actions: deleting or overwriting the user's files, pushing code, publishing, or sending messages on the user's behalf.
- Reply in GitHub-flavored markdown. Keep final answers concise and lead with the result.
`)
	if instructions != "" {
		prompt.WriteString("\n## User instructions\n")
		prompt.WriteString(instructions)
		prompt.WriteString("\n")
	}
	return prompt.String()
}
