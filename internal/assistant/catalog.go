package assistant

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Models.dev's MIT-licensed provider subset. See models-dev.LICENSE.
//
//go:embed models-dev.json
var catalogSnapshot []byte

type ModelInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Context   int    `json:"context"`
	Reasoning bool   `json:"reasoning"`
}

type catalogProvider struct {
	Name   string `json:"name"`
	NPM    string `json:"npm"`
	Models map[string]struct {
		Name      string `json:"name"`
		ToolCall  bool   `json:"tool_call"`
		Reasoning bool   `json:"reasoning"`
		Status    string `json:"status"`
		Provider  struct {
			NPM string `json:"npm"`
			API string `json:"api"`
		} `json:"provider"`
		Limit struct {
			Context int `json:"context"`
		} `json:"limit"`
		Interleaved json.RawMessage `json:"interleaved"`
	} `json:"models"`
}

// Endpoints are trusted adapter configuration, never supplied by remote catalog data.
var adapters = []Preset{
	{ID: "deepseek", Name: "DeepSeek", Endpoint: "https://api.deepseek.com/chat/completions", KeyURL: "https://platform.deepseek.com/api_keys"},
	{ID: "opencode-go", Name: "OpenCode Go", Endpoint: "https://opencode.ai/zen/go/v1/chat/completions", KeyURL: "https://opencode.ai/auth"},
}
var modelIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,99}$`)

func parseCatalog(data []byte) ([]Preset, error) {
	var input map[string]catalogProvider
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, errors.New("invalid Models.dev catalog")
	}
	result := make([]Preset, 0, len(adapters))
	for _, adapter := range adapters {
		source, ok := input[adapter.ID]
		if !ok {
			return nil, errors.New("missing provider in Models.dev catalog")
		}
		p := adapter
		p.Models = []string{}
		p.ModelDetails = []ModelInfo{}
		for id, m := range source.Models {
			npm := source.NPM
			if m.Provider.NPM != "" {
				npm = m.Provider.NPM
			}
			if npm != "@ai-sdk/openai-compatible" || m.Provider.API != "" || !m.ToolCall || m.Status == "deprecated" || !modelIDPattern.MatchString(id) {
				continue
			}
			// This adapter can preserve reasoning_content, but not other reasoning formats.
			if len(m.Interleaved) > 0 && string(m.Interleaved) != "false" && string(m.Interleaved) != "null" {
				var inter struct {
					Field string `json:"field"`
				}
				if json.Unmarshal(m.Interleaved, &inter) != nil || inter.Field != "reasoning_content" {
					continue
				}
			}
			name := m.Name
			if strings.TrimSpace(name) == "" || len(name) > 150 {
				name = id
			}
			p.ModelDetails = append(p.ModelDetails, ModelInfo{ID: id, Name: name, Context: m.Limit.Context, Reasoning: m.Reasoning})
		}
		sort.Slice(p.ModelDetails, func(i, j int) bool { return p.ModelDetails[i].ID < p.ModelDetails[j].ID })
		for _, m := range p.ModelDetails {
			p.Models = append(p.Models, m.ID)
		}
		if len(p.Models) == 0 {
			return nil, errors.New("no compatible tool models in Models.dev catalog")
		}
		result = append(result, p)
	}
	return result, nil
}

func bundledPresets() []Preset {
	p, err := parseCatalog(catalogSnapshot)
	if err != nil {
		panic(err)
	}
	return p
}

// Presets is the immutable bundled fallback, also used to choose the initial model.
var Presets = bundledPresets()

type ModelCatalog struct {
	mu        sync.Mutex
	presets   []Preset
	attempted time.Time
	fetched   time.Time
	Client    *http.Client
}

var Catalog = &ModelCatalog{}

func (c *ModelCatalog) Current() []Preset {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.presets == nil {
		return Presets
	}
	return c.presets
}

// Refresh is bounded and cached, including failures. No credentials are sent to Models.dev.
func (c *ModelCatalog) Refresh(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.attempted) < 24*time.Hour {
		return
	}
	c.attempted = time.Now()
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://models.dev/api.json", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "CasaOS-Assistant/1.0")
	res, err := client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 16*1024*1024+1))
	if err != nil || len(data) > 16*1024*1024 {
		return
	}
	p, err := parseCatalog(data)
	if err != nil {
		return
	}
	c.presets = p
	c.fetched = time.Now()
}
func (c *ModelCatalog) Source() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched.IsZero() {
		return "Models.dev · bundled catalog"
	}
	return "Models.dev · updated " + c.fetched.UTC().Format("2006-01-02")
}
