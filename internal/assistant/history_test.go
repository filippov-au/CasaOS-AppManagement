package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type historyModel struct {
	complete func(Settings, []Message) (Message, error)
}

func (m historyModel) Complete(_ context.Context, cfg Settings, _ string, messages []Message, _ []Tool) (Message, error) {
	return m.complete(cfg, messages)
}

func enableTestHistory(t *testing.T, m *Manager, root string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.EnableHistory(root, func(string) (Settings, error) { return testSettings(), nil }); err != nil {
		t.Fatal(err)
	}
}

func TestHistorySurvivesRestartWithoutCredentialsAndResumesOwnedContext(t *testing.T) {
	root := t.TempDir()
	m := NewManager(&testModel{tool: "inspect"}, &testRuntime{})
	enableTestHistory(t, m, root)
	v, err := m.Start("1", testSettings(), "Investigate the media stack", "review")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, v.ID, "done")
	if _, err = m.AddSecret("1", v.ID, "news_password", "private-news-value"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, v.ID+".json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-news-value", testSettings().APIKey, `"api_key"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("credential persisted", forbidden)
		}
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("history file is not private", err)
	}
	info, err = os.Stat(root)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("history directory is not private", err)
	}
	resumed := NewManager(historyModel{complete: func(cfg Settings, messages []Message) (Message, error) {
		if cfg.APIKey != testSettings().APIKey {
			return Message{}, errors.New("provider key was not restored from settings")
		}
		b, _ := json.Marshal(messages)
		if !strings.Contains(string(b), "Investigate the media stack") || !strings.Contains(string(b), "Check again") {
			return Message{}, errors.New("conversation context missing")
		}
		return Message{Role: "assistant", Content: "Continued with earlier context."}, nil
	}}, &testRuntime{})
	enableTestHistory(t, resumed, root)
	if len(resumed.List("1")) != 1 || len(resumed.List("2")) != 0 {
		t.Fatal("topic ownership lost")
	}
	if _, err = resumed.Get("2", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-user restored history")
	}
	s, _ := resumed.get("1", v.ID)
	if len(s.secrets) != 0 || s.cfg.APIKey != "" {
		t.Fatal("restored a private credential")
	}
	if _, err = resumed.Reply("1", v.ID, "Check again"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, resumed, v.ID, "done")
	if err = resumed.Delete("1", v.ID); err != nil {
		t.Fatal(err)
	}
	afterDelete := NewManager(&testModel{}, &testRuntime{})
	enableTestHistory(t, afterDelete, root)
	if len(afterDelete.List("1")) != 0 {
		t.Fatal("deleted conversation resurrected")
	}
}

func TestRestartInvalidatesApprovalAndNeverReplaysAnInterruptedWrite(t *testing.T) {
	root := t.TempDir()
	runtime := &testRuntime{}
	m := NewManager(&testModel{tool: "change"}, runtime)
	enableTestHistory(t, m, root)
	v, err := m.Start("1", testSettings(), "Configure Sonarr", "review")
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, m, v.ID, "approval")
	data, err := os.ReadFile(filepath.Join(root, v.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), v.Pending.ID) {
		t.Fatal("approval capability was persisted")
	}
	for _, status := range []string{"approval", "running"} {
		t.Run(status, func(t *testing.T) {
			copyRoot := t.TempDir()
			var record historyRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			record.View.Status = status
			b, _ := json.Marshal(record)
			if err := os.WriteFile(filepath.Join(copyRoot, v.ID+".json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			r := &testRuntime{}
			model := &testModel{tool: "inspect"}
			restored := NewManager(model, r)
			enableTestHistory(t, restored, copyRoot)
			got, err := restored.Get("1", v.ID)
			if err != nil || got.Status != "done" || got.Pending != nil {
				t.Fatal("interrupted conversation not recoverable", got, err)
			}
			if _, err = restored.Approve("1", v.ID, v.Pending.ID, true); !errors.Is(err, ErrBusy) {
				t.Fatal("replayed old approval")
			}
			if model.calls != 0 || r.prepared != 0 || r.writes != 0 {
				t.Fatal("restart started external work")
			}
			s, _ := restored.get("1", v.ID)
			toolResult := false
			for _, message := range s.messages {
				if message.Role == "tool" && message.CallID == "call1" && strings.Contains(message.Content, "may or may not") {
					toolResult = true
				}
			}
			if !toolResult {
				t.Fatal("unanswered tool call was not closed with uncertainty")
			}
		})
	}
	if runtime.writes != 0 {
		t.Fatal("write executed during review")
	}
}

func TestHistoryFailureStopsBeforeExternalAction(t *testing.T) {
	root := t.TempDir()
	r := &testRuntime{}
	m := NewManager(&testModel{tool: "change"}, r)
	enableTestHistory(t, m, root)
	v, err := m.Start("1", testSettings(), "Configure app", "review")
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, m, v.ID, "approval")
	// Replace the history directory with a regular file to simulate lost storage.
	if err = os.Rename(root, root+"-saved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(root); _ = os.Rename(root+"-saved", root) })
	if err = os.WriteFile(root, []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Approve("1", v.ID, v.Pending.ID, true); !errors.Is(err, ErrHistory) {
		t.Fatal("storage failure ignored", err)
	}
	if r.writes != 0 {
		t.Fatal("write ran without an audit trail")
	}
	waitStatus(t, m, v.ID, "error")
}

func TestHistoryRejectsSymlinksAndCorruptRecords(t *testing.T) {
	for _, mode := range []string{"symlink", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, ID()+".json")
			if mode == "symlink" {
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), file); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(file, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			m := NewManager(&testModel{}, &testRuntime{})
			if err := m.EnableHistory(root, nil); !errors.Is(err, ErrHistory) {
				t.Fatal("invalid history accepted", err)
			}
			if _, err := m.Start("1", testSettings(), "Hello", "read"); !errors.Is(err, ErrHistory) {
				t.Fatal("started after failed history recovery", err)
			}
		})
	}
}

func TestRestoredConversationNeverSwitchesProviderSilently(t *testing.T) {
	root := t.TempDir()
	m := NewManager(&testModel{tool: "inspect"}, &testRuntime{})
	enableTestHistory(t, m, root)
	v, err := m.Start("1", testSettings(), "Inspect", "read")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, v.ID, "done")
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	model := &testModel{tool: "inspect"}
	restored := NewManager(model, &testRuntime{})
	if err = restored.EnableHistory(root, func(string) (Settings, error) {
		return Settings{Provider: "opencode-go", Model: Presets[1].Models[0], APIKey: "different-provider-key"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = restored.Reply("1", v.ID, "Continue"); err == nil {
		t.Fatal("conversation silently changed providers")
	}
	if model.calls != 0 {
		t.Fatal("contacted another provider")
	}
}
