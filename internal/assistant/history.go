package assistant

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ErrHistory = errors.New("conversation history is unavailable; check the assistant storage directory")
var historyID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type historyRecord struct {
	Version  int       `json:"version"`
	Owner    string    `json:"owner"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	View     View      `json:"view"`
	Messages []Message `json:"messages"`
	Touched  time.Time `json:"touched"`
}

// EnableHistory runs before serving requests. No credentials, executable plans,
// approval authority, or worker goroutines are restored from disk.
func (m *Manager) EnableHistory(root string, settings func(string) (Settings, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.historyRoot != "" {
		return m.historyErr
	}
	m.historyRoot, m.settings = root, settings
	fail := func() error { m.historyErr = ErrHistory; return ErrHistory }
	if root == "" || privateHistoryPath(root) != nil || os.MkdirAll(root, 0700) != nil || os.Chmod(root, 0700) != nil {
		return fail()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fail()
	}
	restored := map[string]*session{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		file := filepath.Join(root, entry.Name())
		info, err := os.Lstat(file)
		if err != nil || !historyID.MatchString(id) || !info.Mode().IsRegular() || info.Size() > 8*1024*1024 || len(restored) >= 128 {
			return fail()
		}
		data, err := os.ReadFile(file)
		var record historyRecord
		if err != nil || json.Unmarshal(data, &record) != nil || record.Version != 1 || record.View.ID != id || !ownerPattern.MatchString(record.Owner) || len(record.Messages) > 200 || len(record.View.Events) > 2000 {
			return fail()
		}
		if record.View.Mode != "read" && record.View.Mode != "review" && record.View.Mode != "auto" {
			return fail()
		}
		if record.View.Status != "running" && record.View.Status != "approval" && record.View.Status != "done" && record.View.Status != "error" && record.View.Status != "cancelled" {
			return fail()
		}
		s := &session{view: record.View, owner: record.Owner, cfg: Settings{Provider: record.Provider, Model: record.Model}, messages: record.Messages, touched: record.Touched, historyPath: file}
		s.view.Provider, s.view.Model = record.Provider, record.Model
		s.view.Pending = nil
		if s.view.Status == "running" || s.view.Status == "approval" {
			// Every unfinished tool call needs a result before the next completion.
			// The operation may have reached Docker; its effects are unknown here.
			pending := []Call{}
			for _, message := range s.messages {
				if message.Role == "assistant" {
					pending = append(pending, message.Calls...)
				}
				if message.Role == "tool" {
					for i, call := range pending {
						if call.ID == message.CallID {
							pending = append(pending[:i], pending[i+1:]...)
							break
						}
					}
				}
			}
			for _, call := range pending {
				s.messages = append(s.messages, Message{Role: "tool", CallID: call.ID, Content: "AppManagement restarted before this result was recorded. The operation may or may not have changed app state. Inspect current state before proposing another change; previous approval does not authorize a retry."})
			}
			s.view.Status = "done"
			notice := "AppManagement restarted. No pending changes were replayed. Inspect current app state before continuing; add private credentials again if needed."
			s.event("status", notice, "")
			s.messages = append(s.messages, Message{Role: "system", Content: notice})
		}
		if len(s.messages) > 0 && s.messages[0].Role == "system" {
			s.messages[0].Content = systemPrompt
		}
		if err := s.checkpoint(); err != nil {
			return fail()
		}
		restored[id] = s
	}
	m.sessions = restored
	return nil
}

func privateHistoryPath(path string) error {
	for path != "." && path != "/" {
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return ErrHistory
		}
		path = filepath.Dir(path)
	}
	return nil
}

// redactHistory also removes newly registered secrets from older messages before
// they can be replayed to a provider or written to durable history.
func (s *session) redactHistory() {
	for i := range s.view.Events {
		s.view.Events[i].Text = s.redact(s.view.Events[i].Text)
	}
	for i := range s.messages {
		s.messages[i].Content = s.redact(s.messages[i].Content)
		s.messages[i].Reasoning = s.redact(s.messages[i].Reasoning)
		for j := range s.messages[i].Calls {
			s.messages[i].Calls[j].Function.Arguments = s.redact(s.messages[i].Calls[j].Function.Arguments)
		}
	}
}

// checkpoint is called with the session locked, before external work and after
// state transitions. Failure stops the worker instead of losing its audit trail.
func (s *session) checkpoint() error {
	if s.historyPath == "" || s.deleted {
		return nil
	}
	s.redactHistory()
	if err := s.saveHistory(); err != nil {
		s.view.Status = "error"
		s.view.Pending, s.prepared = nil, nil
		s.event("error", "Could not save conversation history. Work stopped; inspect current app state before retrying.", "")
		return ErrHistory
	}
	return nil
}

func (s *session) saveHistory() error {
	if err := privateHistoryPath(s.historyPath); err != nil {
		return err
	}
	view := s.snapshot()
	// Pending summaries remain in the event trail, but approval IDs are ephemeral.
	if view.Pending != nil {
		view.Pending.ID = ""
	}
	record := historyRecord{1, s.owner, s.cfg.Provider, s.cfg.Model, view, s.messages, s.touched}
	data, err := json.Marshal(record)
	if err != nil || len(data) > 8*1024*1024 {
		return ErrHistory
	}
	dir := filepath.Dir(s.historyPath)
	f, err := os.CreateTemp(dir, ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.historyPath); err != nil {
		return err
	}
	return syncHistoryDir(dir)
}

func syncHistoryDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (m *Manager) restoreKey(s *session) error {
	if s.cfg.APIKey != "" {
		return nil
	}
	if m.settings == nil {
		return errors.New("configure an AI provider first")
	}
	cfg, err := m.settings(s.owner)
	if m.ResolveSettings != nil {
		cfg, err = m.ResolveSettings(s.owner, s.cfg.Provider)
	}
	if err != nil || cfg.APIKey == "" || cfg.Provider != s.cfg.Provider {
		return errors.New("connect this conversation's provider in AI settings, or start a new chat with your selected provider")
	}
	if _, err := endpoint(s.cfg); err != nil {
		return errors.New("this conversation's model is no longer available; start a new chat")
	}
	s.cfg.APIKey = cfg.APIKey
	return nil
}
