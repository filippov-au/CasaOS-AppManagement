// Package assistant implements a bounded, tool-driven CasaOS assistant.
package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Preset struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Endpoint     string      `json:"endpoint"`
	Models       []string    `json:"models"`
	ModelDetails []ModelInfo `json:"model_details"`
	KeyURL       string      `json:"key_url"`
}

type Settings struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	APIKey     string `json:"api_key,omitempty"`
	Configured bool   `json:"configured"`
}

func endpoint(cfg Settings) (string, error) {
	for _, p := range Catalog.Current() {
		if p.ID == cfg.Provider {
			for _, model := range p.Models {
				if model == cfg.Model {
					return p.Endpoint, nil
				}
			}
		}
	}
	return "", errors.New("choose a supported provider and model")
}

type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type Call struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Message struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning_content,omitempty"`
	Calls     []Call `json:"tool_calls,omitempty"`
	CallID    string `json:"tool_call_id,omitempty"`
}
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}
type Model interface {
	Complete(context.Context, Settings, string, []Message, []Tool) (Message, error)
}
type ChatModel struct{ Client *http.Client }

func (p ChatModel) Complete(ctx context.Context, cfg Settings, session string, messages []Message, tools []Tool) (Message, error) {
	url, err := endpoint(cfg)
	if err != nil {
		return Message{}, err
	}
	if cfg.APIKey == "" {
		return Message{}, errors.New("add your provider API key in AI settings")
	}
	body := map[string]interface{}{"model": cfg.Model, "messages": messages, "max_tokens": 4096, "stream": false}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CasaOS-Assistant/1.0")
	req.Header.Set("x-opencode-session", session)
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	res, err := client.Do(req)
	if err != nil {
		return Message{}, errors.New("provider request failed; check connectivity or retry")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("provider returned HTTP %d; check API key, model access, and usage limits", res.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, 1024*1024+1))
	if err != nil {
		return Message{}, err
	}
	if len(data) > 1024*1024 {
		return Message{}, errors.New("provider response exceeded limit")
	}
	var response struct {
		Choices []struct {
			Message Message `json:"message"`
			Finish  string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err = json.Unmarshal(data, &response); err != nil || len(response.Choices) == 0 {
		return Message{}, errors.New("provider returned an invalid completion")
	}
	if response.Choices[0].Finish == "length" {
		return Message{}, errors.New("provider output was truncated; try a smaller task")
	}
	msg := response.Choices[0].Message
	if msg.Role != "assistant" || (msg.Content == "" && len(msg.Calls) == 0) {
		return Message{}, errors.New("provider returned an empty completion")
	}
	return msg, nil
}

var secretPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|token|password|passwd|secret|authorization|controlpassword)\s*["']?\s*[:=]\s*["']?)([^\s,"'<>]+)`)
var bearerPattern = regexp.MustCompile(`(?i)(bearer|basic)\s+[a-z0-9+/=._-]+`)
var userinfoPattern = regexp.MustCompile(`(https?://)[^\s/@]+:[^\s/@]+@`)

var secretReference = regexp.MustCompile(`\$secret:[a-z][a-z0-9_]{0,47}`)

func Redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	references := []string{}
	text = secretReference.ReplaceAllStringFunc(text, func(ref string) string {
		references = append(references, ref)
		return fmt.Sprintf("\x1eCREDENTIAL%d\x1e", len(references)-1)
	})
	text = bearerPattern.ReplaceAllString(text, "${1} [redacted]")
	text = secretPattern.ReplaceAllString(text, "${1}[redacted]")
	text = userinfoPattern.ReplaceAllString(text, "${1}[redacted]@")
	for i, ref := range references {
		text = strings.ReplaceAll(text, fmt.Sprintf("\x1eCREDENTIAL%d\x1e", i), ref)
	}
	return text
}
