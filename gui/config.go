package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/fireworks"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/ollama"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openrouter"
)

// secretMask stands in for stored secrets in API responses. Updates that send
// it back keep the stored value.
const secretMask = "••••••••"

var thinkingLevels = []string{"low", "medium", "high", "xhigh", "max"}

type Config struct {
	Provider     string            `json:"provider"`
	Models       map[string]string `json:"models"`
	BaseURLs     map[string]string `json:"base_urls"`
	APIKeys      map[string]string `json:"api_keys"`
	Thinking     string            `json:"thinking"`
	WebSearch    bool              `json:"web_search"`
	Workspace    string            `json:"workspace"`
	Instructions string            `json:"instructions"`
	Env          map[string]string `json:"env"`
	// Permission is "ask" (approve each shell command) or "auto".
	Permission string `json:"permission"`
	// Enabled opts in to providers that need no key (Codex login, Ollama).
	Enabled map[string]bool `json:"enabled"`
}

type configUpdate struct {
	Provider     *string           `json:"provider"`
	Models       map[string]string `json:"models"`
	BaseURLs     map[string]string `json:"base_urls"`
	APIKeys      map[string]string `json:"api_keys"`
	Thinking     *string           `json:"thinking"`
	WebSearch    *bool             `json:"web_search"`
	Workspace    *string           `json:"workspace"`
	Instructions *string           `json:"instructions"`
	Env          map[string]string `json:"env"`
	Permission   *string           `json:"permission"`
	Enabled      map[string]bool   `json:"enabled"`
}

type client interface {
	llm.Adapter
	Close() error
}

// provider mirrors the table in cmd/internal/agentrunner/providers.go, which
// is internal to the upstream module. TestProvidersMatchUpstream fails when
// upstream adds a provider this table lacks.
type provider struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	BaseURL string `json:"base_url"`
	KeyEnv  string `json:"key_env"`
	// Models are the choices offered in the toolbar, cheapest first. Providers
	// without a curated list take a model ID in Settings.
	Models       []modelOption `json:"models"`
	HostedSearch bool          `json:"hosted_search"`
	newClient    func(key, baseURL string) (client, error)
}

type modelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// OpenRouter comes first: one key reaches most models. Each curated list pairs
// the cheapest model that handles the harness's tool calls with GPT-6 Astra,
// the model Unreal Labs benchmarked the harness with.
var providers = []provider{
	{
		Name: "openrouter", Label: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1",
		KeyEnv: "OPENROUTER_API_KEY",
		Models: []modelOption{
			{ID: "nvidia/nemotron-3-super-120b-a12b:free", Label: "Nemotron 3 Super · free"},
			{ID: "openai/gpt-6-astra", Label: "GPT-6 Astra · best"},
		},
		newClient: func(key, baseURL string) (client, error) {
			return openrouter.NewClient(openrouter.Config{APIKey: key, BaseURL: baseURL})
		},
	},
	{
		Name: "openai", Label: "OpenAI", BaseURL: "https://api.openai.com/v1",
		KeyEnv: "OPENAI_API_KEY", HostedSearch: true,
		Models: []modelOption{
			{ID: "gpt-6-luna", Label: "GPT-6 Luna · cheaper"},
			{ID: "gpt-6-astra", Label: "GPT-6 Astra · best"},
		},
		newClient: func(key, baseURL string) (client, error) {
			return openai.NewClient(openai.Config{APIKey: key, BaseURL: baseURL})
		},
	},
	{
		Name: "openai-codex", Label: "ChatGPT (Codex login)", BaseURL: openaicodex.BaseURL, HostedSearch: true,
		Models: []modelOption{
			{ID: "gpt-6-sol", Label: "GPT-6 Sol"},
			{ID: "gpt-6-astra", Label: "GPT-6 Astra · best"},
		},
		newClient: func(_, baseURL string) (client, error) {
			config, err := openaicodex.EnvironmentConfig(os.Getenv)
			if err != nil {
				return nil, err
			}
			config.BaseURL = baseURL
			return openaicodex.NewClient(config)
		},
	},
	{
		Name: "fireworks", Label: "Fireworks", BaseURL: "https://api.fireworks.ai/inference/v1",
		KeyEnv: "FIREWORKS_API_KEY",
		newClient: func(key, baseURL string) (client, error) {
			return fireworks.NewClient(fireworks.Config{APIKey: key, BaseURL: baseURL})
		},
	},
	{
		Name: "ollama", Label: "Ollama (local)", BaseURL: ollama.BaseURL,
		newClient: func(_, baseURL string) (client, error) {
			return ollama.NewClient(ollama.Config{BaseURL: baseURL})
		},
	},
}

func providerNamed(name string) (provider, bool) {
	index := slices.IndexFunc(providers, func(p provider) bool { return p.Name == name })
	if index < 0 {
		return provider{}, false
	}
	return providers[index], true
}

func (p provider) baseURL(cfg Config) string {
	if override := strings.TrimSpace(cfg.BaseURLs[p.Name]); override != "" {
		return override
	}
	return p.BaseURL
}

func (p provider) apiKey(cfg Config) string {
	if key := strings.TrimSpace(cfg.APIKeys[p.Name]); key != "" {
		return key
	}
	if p.KeyEnv != "" {
		return strings.TrimSpace(os.Getenv(p.KeyEnv))
	}
	return ""
}

// model is the selected model: one of the curated options (the cheapest by
// default), or the ID typed in Settings for providers without a list.
func (p provider) model(cfg Config) string {
	chosen := strings.TrimSpace(cfg.Models[p.Name])
	if len(p.Models) == 0 {
		return chosen
	}
	if slices.ContainsFunc(p.Models, func(m modelOption) bool { return m.ID == chosen }) {
		return chosen
	}
	return p.Models[0].ID
}

// available reports whether the provider can be selected: it has a key, or
// the user enabled a keyless provider, and it has a model.
func (p provider) available(cfg Config) bool {
	if p.KeyEnv != "" {
		if p.apiKey(cfg) == "" {
			return false
		}
	} else if !cfg.Enabled[p.Name] {
		return false
	}
	return p.model(cfg) != ""
}

// selectedProvider is the configured provider if available, otherwise the
// first available one.
func (cfg Config) selectedProvider() (provider, bool) {
	if p, ok := providerNamed(cfg.Provider); ok && p.available(cfg) {
		return p, true
	}
	for _, p := range providers {
		if p.available(cfg) {
			return p, true
		}
	}
	return provider{}, false
}

func (p provider) connect(cfg Config) (client, error) {
	key := p.apiKey(cfg)
	if p.KeyEnv != "" && key == "" {
		return nil, fmt.Errorf("%s needs an API key: add it in Settings or set %s", p.Label, p.KeyEnv)
	}
	c, err := p.newClient(key, p.baseURL(cfg))
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", p.Label, err)
	}
	return c, nil
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Provider:   "openrouter",
		Thinking:   "high",
		WebSearch:  true,
		Permission: permissionAsk,
		Workspace:  filepath.Join(home, "UnrealAgent"),
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	encoded, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(encoded, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

func saveConfig(path string, cfg Config) error {
	encoded, err := json.Marshal(cfg, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	return writeFileAtomic(path, encoded, 0o600)
}

// view is the config as sent to the browser, without secrets.
func (cfg Config) view() map[string]any {
	keys := make(map[string]string)
	for _, p := range providers {
		switch {
		case strings.TrimSpace(cfg.APIKeys[p.Name]) != "":
			keys[p.Name] = "saved"
		case p.KeyEnv != "" && os.Getenv(p.KeyEnv) != "":
			keys[p.Name] = "env"
		}
	}
	env := make(map[string]string, len(cfg.Env))
	for name := range cfg.Env {
		env[name] = secretMask
	}
	available := []string{}
	for _, p := range providers {
		if p.available(cfg) {
			available = append(available, p.Name)
		}
	}
	selected := map[string]string{}
	for _, p := range providers {
		selected[p.Name] = p.model(cfg)
	}
	current, _ := cfg.selectedProvider()
	return map[string]any{
		"provider":     current.Name,
		"available":    available,
		"selected":     selected,
		"permission":   cfg.Permission,
		"enabled":      nonNilBools(cfg.Enabled),
		"models":       nonNil(cfg.Models),
		"base_urls":    nonNil(cfg.BaseURLs),
		"key_status":   keys,
		"thinking":     cfg.Thinking,
		"web_search":   cfg.WebSearch,
		"workspace":    cfg.Workspace,
		"instructions": cfg.Instructions,
		"env":          env,
	}
}

func (cfg Config) apply(update configUpdate) (Config, error) {
	next := cfg
	next.Models = mergeStrings(cfg.Models, update.Models)
	next.BaseURLs = mergeStrings(cfg.BaseURLs, update.BaseURLs)
	next.APIKeys = cloneStrings(cfg.APIKeys)
	for name, key := range update.APIKeys {
		switch key = strings.TrimSpace(key); key {
		case "", secretMask:
		case "-":
			delete(next.APIKeys, name)
		default:
			next.APIKeys[name] = key
		}
	}
	if update.Provider != nil {
		if _, ok := providerNamed(*update.Provider); !ok {
			return cfg, fmt.Errorf("unknown provider %q", *update.Provider)
		}
		next.Provider = *update.Provider
	}
	if update.Thinking != nil {
		if !slices.Contains(thinkingLevels, *update.Thinking) {
			return cfg, fmt.Errorf("thinking must be one of %s", strings.Join(thinkingLevels, ", "))
		}
		next.Thinking = *update.Thinking
	}
	if update.WebSearch != nil {
		next.WebSearch = *update.WebSearch
	}
	if update.Workspace != nil {
		workspace, err := resolveWorkspace(*update.Workspace)
		if err != nil {
			return cfg, err
		}
		next.Workspace = workspace
	}
	if update.Instructions != nil {
		next.Instructions = strings.TrimSpace(*update.Instructions)
	}
	if update.Permission != nil {
		if *update.Permission != permissionAsk && *update.Permission != permissionAuto {
			return cfg, fmt.Errorf("permission must be %q or %q", permissionAsk, permissionAuto)
		}
		next.Permission = *update.Permission
	}
	if update.Enabled != nil {
		next.Enabled = make(map[string]bool, len(cfg.Enabled))
		for name, on := range cfg.Enabled {
			next.Enabled[name] = on
		}
		for name, on := range update.Enabled {
			next.Enabled[name] = on
		}
	}
	if update.Env != nil {
		next.Env = make(map[string]string, len(update.Env))
		for name, value := range update.Env {
			name = strings.TrimSpace(name)
			if name == "" || strings.ContainsAny(name, "= \t\n") {
				return cfg, fmt.Errorf("invalid environment variable name %q", name)
			}
			if value == secretMask {
				value = cfg.Env[name]
			}
			next.Env[name] = value
		}
	}
	return next, nil
}

// resolveWorkspace expands ~ and requires an existing directory.
func resolveWorkspace(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if path == "" {
		return "", errors.New("workspace must be set")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", absolute)
	}
	return absolute, nil
}

func mergeStrings(current, update map[string]string) map[string]string {
	merged := cloneStrings(current)
	for key, value := range update {
		if value = strings.TrimSpace(value); value == "" {
			delete(merged, key)
		} else {
			merged[key] = value
		}
	}
	return merged
}

func cloneStrings(source map[string]string) map[string]string {
	copied := make(map[string]string, len(source))
	for key, value := range source {
		copied[key] = value
	}
	return copied
}

func nonNilBools(source map[string]bool) map[string]bool {
	if source == nil {
		return map[string]bool{}
	}
	return source
}

func nonNil(source map[string]string) map[string]string {
	if source == nil {
		return map[string]string{}
	}
	return source
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
