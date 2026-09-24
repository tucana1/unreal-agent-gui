package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// Models are recommended picks, cheapest first; the first is the default.
	Models []modelOption `json:"models"`
	// Catalog means the provider lists its models at GET <base URL>/models,
	// so the toolbar can offer any of them.
	Catalog      bool `json:"catalog"`
	HostedSearch bool `json:"hosted_search"`
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
		KeyEnv: "OPENROUTER_API_KEY", Catalog: true,
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
		KeyEnv: "OPENAI_API_KEY", HostedSearch: true, Catalog: true,
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
		KeyEnv: "FIREWORKS_API_KEY", Catalog: true,
		newClient: func(key, baseURL string) (client, error) {
			return fireworks.NewClient(fireworks.Config{APIKey: key, BaseURL: baseURL})
		},
	},
	{
		Name: "ollama", Label: "Ollama (local)", BaseURL: ollama.BaseURL, Catalog: true,
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

// model is the chosen model ID, or the first recommended pick.
func (p provider) model(cfg Config) string {
	if chosen := strings.TrimSpace(cfg.Models[p.Name]); chosen != "" {
		return chosen
	}
	if len(p.Models) > 0 {
		return p.Models[0].ID
	}
	return ""
}

// available reports whether the provider can be selected: it has a key, or
// the user enabled a keyless provider.
func (p provider) available(cfg Config) bool {
	if p.KeyEnv != "" {
		return p.apiKey(cfg) != ""
	}
	return cfg.Enabled[p.Name]
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

// modelInfo describes one model in a provider's catalog. Prices are USD per
// million tokens; -1 means unknown (plain /models lists carry no metadata).
type modelInfo struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	Context    int     `json:"context"`
	Reasoning  bool    `json:"reasoning"`
	Recommends bool    `json:"recommended"`
}

type catalogEntry struct {
	models  []modelInfo
	fetched time.Time
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]catalogEntry{}
)

const catalogTTL = 30 * time.Minute

// models returns the provider's catalog, limited to models that can call
// tools, which the agent needs. Results are cached briefly.
func (p provider) models(ctx context.Context, cfg Config) ([]modelInfo, error) {
	if !p.Catalog {
		return p.recommended(), nil
	}
	catalogMu.Lock()
	cached, ok := catalogCache[p.Name]
	catalogMu.Unlock()
	if ok && time.Since(cached.fetched) < catalogTTL {
		return cached.models, nil
	}
	models, err := p.fetchModels(ctx, cfg)
	if err != nil {
		return p.recommended(), err
	}
	catalogMu.Lock()
	catalogCache[p.Name] = catalogEntry{models: models, fetched: time.Now()}
	catalogMu.Unlock()
	return models, nil
}

func (p provider) recommended() []modelInfo {
	models := make([]modelInfo, 0, len(p.Models))
	for _, option := range p.Models {
		models = append(models, modelInfo{ID: option.ID, Name: option.Label, Input: -1, Output: -1, Reasoning: true, Recommends: true})
	}
	return models
}

func (p provider) fetchModels(ctx context.Context, cfg Config) ([]modelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.baseURL(cfg), "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key := p.apiKey(cfg); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list %s models: %w", p.Label, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s models: HTTP %d", p.Label, response.StatusCode)
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Context int    `json:"context_length"`
			Pricing *struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Parameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.UnmarshalRead(io.LimitReader(response.Body, 32<<20), &body); err != nil {
		return nil, fmt.Errorf("decode %s models: %w", p.Label, err)
	}
	recommended := map[string]bool{}
	for _, option := range p.Models {
		recommended[option.ID] = true
	}
	models := make([]modelInfo, 0, len(body.Data))
	for _, entry := range body.Data {
		if entry.ID == "" || strings.HasSuffix(entry.ID, ":batch") {
			continue
		}
		info := modelInfo{ID: entry.ID, Name: entry.Name, Input: -1, Output: -1, Context: entry.Context, Recommends: recommended[entry.ID]}
		if entry.Parameters != nil {
			// Rich catalogs (OpenRouter) say which models can call tools.
			if !slices.Contains(entry.Parameters, "tools") {
				continue
			}
			info.Reasoning = slices.Contains(entry.Parameters, "reasoning")
		} else {
			info.Reasoning = true // unknown; the effort setting is harmless if ignored
		}
		if entry.Pricing != nil {
			info.Input, info.Output = perMillion(entry.Pricing.Prompt), perMillion(entry.Pricing.Completion)
		}
		models = append(models, info)
	}
	slices.SortFunc(models, func(a, b modelInfo) int { return strings.Compare(a.ID, b.ID) })
	return models, nil
}

// perMillion converts a per-token price string to USD per million tokens.
func perMillion(price string) float64 {
	value, err := strconv.ParseFloat(price, 64)
	if err != nil || value < 0 {
		return -1
	}
	return math.Round(value*1e12) / 1e6 // six decimals, without float noise
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
