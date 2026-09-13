package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testModel struct {
	mu       sync.Mutex
	calls    int
	received []Message
	tool     string
}

func (m *testModel) Complete(ctx context.Context, c Settings, id string, msg []Message, tools []Tool) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.received = append([]Message{}, msg...)
	if m.calls == 1 {
		return Message{Role: "assistant", Calls: []Call{{ID: "call1", Type: "function", Function: Function{Name: m.tool, Arguments: `{}`}}}}, nil
	}
	return Message{Role: "assistant", Content: "Finished after checking tool evidence."}, nil
}

type testRuntime struct {
	mu       sync.Mutex
	writes   int
	prepared int
	output   string
	started  chan struct{}
}

func (r *testRuntime) Tools() []Tool {
	return []Tool{{Function: ToolFunction{Name: "change"}}, {Function: ToolFunction{Name: "inspect"}}}
}
func (r *testRuntime) IsWrite(n string) bool { return n == "change" }
func (r *testRuntime) Prepare(ctx context.Context, n string, raw json.RawMessage) (Prepared, error) {
	r.mu.Lock()
	r.prepared++
	r.mu.Unlock()
	return Prepared{Summary: "Change an app setting", Execute: func(ctx context.Context) (string, error) {
		if r.started != nil {
			close(r.started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if n == "change" {
			r.writes++
		}
		return r.output, nil
	}}, nil
}
func waitStatus(t *testing.T, m *Manager, id string, status string) View {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, e := m.Get("1", id)
		if e != nil {
			t.Fatal(e)
		}
		if v.Status == status {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	v, _ := m.Get("1", id)
	t.Fatalf("want %s got %+v", status, v)
	return View{}
}
func testSettings() Settings {
	return Settings{Provider: "deepseek", Model: Presets[0].Models[0], APIKey: "test-provider-secret"}
}
func TestReviewApprovalIsOwnedSingleUseAndResumes(t *testing.T) {
	model := &testModel{tool: "change"}
	runtime := &testRuntime{output: "setting applied"}
	m := NewManager(model, runtime)
	v, e := m.Start("1", testSettings(), "fix app", "review")
	if e != nil {
		t.Fatal(e)
	}
	v = waitStatus(t, m, v.ID, "approval")
	if runtime.writes != 0 {
		t.Fatal("wrote before approval")
	}
	if _, e = m.Get("2", v.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("cross-user read")
	}
	if _, e = m.Approve("2", v.ID, v.Pending.ID, true); !errors.Is(e, ErrNotFound) {
		t.Fatal("cross-user approval")
	}
	if _, e = m.Approve("1", v.ID, "wrong", true); !errors.Is(e, ErrBusy) {
		t.Fatal("accepted stale approval")
	}
	pending := v.Pending.ID
	if _, e = m.Approve("1", v.ID, pending, true); e != nil {
		t.Fatal(e)
	}
	waitStatus(t, m, v.ID, "done")
	if _, e = m.Approve("1", v.ID, pending, true); !errors.Is(e, ErrBusy) {
		t.Fatal("replayed approval")
	}
	if runtime.writes != 1 {
		t.Fatal("write count", runtime.writes)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if model.received[len(model.received)-1].Content != "setting applied" {
		t.Fatal("missing evidence in model context")
	}
}
func TestReadModeAndDeclineNeverExecuteWrites(t *testing.T) {
	for _, mode := range []string{"read", "review"} {
		t.Run(mode, func(t *testing.T) {
			r := &testRuntime{}
			m := NewManager(&testModel{tool: "change"}, r)
			v, e := m.Start("1", testSettings(), "fix", ""+mode)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "review" {
				v = waitStatus(t, m, v.ID, "approval")
				_, e = m.Approve("1", v.ID, v.Pending.ID, false)
				if e != nil {
					t.Fatal(e)
				}
			}
			waitStatus(t, m, v.ID, "done")
			if r.writes != 0 {
				t.Fatal("unexpected write")
			}
		})
	}
}
func TestAutomaticModeAndUnknownTool(t *testing.T) {
	for _, tool := range []string{"change", "shell"} {
		t.Run(tool, func(t *testing.T) {
			r := &testRuntime{}
			m := NewManager(&testModel{tool: tool}, r)
			v, e := m.Start("1", testSettings(), "setup", "auto")
			if e != nil {
				t.Fatal(e)
			}
			waitStatus(t, m, v.ID, "done")
			expected := 0
			if tool == "change" {
				expected = 1
			}
			if r.writes != expected {
				t.Fatal("unexpected writes")
			}
		})
	}
}
func TestCancellationInterruptsTool(t *testing.T) {
	r := &testRuntime{started: make(chan struct{})}
	m := NewManager(&testModel{tool: "inspect"}, r)
	v, e := m.Start("1", testSettings(), "inspect", "read")
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-r.started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	if _, e = m.Cancel("1", v.ID); e != nil {
		t.Fatal(e)
	}
	waitStatus(t, m, v.ID, "cancelled")
}
func TestSettingsPrivateKeyPreservedAndProviderSwitchClears(t *testing.T) {
	s := &SettingsStore{Root: t.TempDir()}
	cfg := testSettings()
	saved, e := s.Save("1", cfg)
	if e != nil {
		t.Fatal(e)
	}
	if saved.APIKey != "" || !saved.Configured {
		t.Fatal("key exposed")
	}
	info, e := os.Stat(filepath.Join(s.Root, "1.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	saved, e = s.Save("1", Settings{Provider: cfg.Provider, Model: cfg.Model})
	if e != nil || !saved.Configured {
		t.Fatal("key was lost")
	}
	read, _ := s.Read("1")
	if read.APIKey != cfg.APIKey {
		t.Fatal("stored key changed")
	}
	saved, e = s.Save("1", Settings{Provider: "opencode-go", Model: Presets[1].Models[0]})
	if e != nil || saved.Configured {
		t.Fatal("key reused for another provider")
	}
	if _, e = s.Read("../1"); e == nil {
		t.Fatal("path traversal accepted")
	}
	if e = s.Delete("1"); e != nil {
		t.Fatal(e)
	}
}
func TestRedaction(t *testing.T) {
	text := Redact(`API_KEY=abc123 password: hunter2 Authorization: Bearer eyJsecret https://name:pass@host/ test-provider-secret`, "test-provider-secret")
	for _, s := range []string{"abc123", "hunter2", "eyJsecret", "name:pass", "test-provider-secret"} {
		if strings.Contains(text, s) {
			t.Fatalf("leaked %s: %s", s, text)
		}
	}
}

func TestTopicsAreOwnedAndKeepEarlierConversations(t *testing.T) {
	m := NewManager(&testModel{tool: "inspect"}, &testRuntime{})
	first, err := m.Start("1", testSettings(), "First topic", "read")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, first.ID, "done")
	second, err := m.Start("1", testSettings(), "Second topic", "read")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, second.ID, "done")
	if len(m.List("2")) != 0 {
		t.Fatal("cross-user topics")
	}
	topics := m.List("1")
	if len(topics) != 2 || topics[0].Title != "Second topic" {
		t.Fatal(topics)
	}
	if err = m.Delete("1", first.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.List("1")) != 1 {
		t.Fatal("delete did not remove topic")
	}
}

func TestReplyCannotStartAnotherRunWhileAnApprovalIsPending(t *testing.T) {
	m := NewManager(&testModel{tool: "inspect"}, &testRuntime{})
	first, err := m.Start("1", testSettings(), "Inspect", "review")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, first.ID, "done")
	m.Model = &testModel{tool: "change"}
	second, err := m.Start("1", testSettings(), "Change an app", "review")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, second.ID, "approval")
	if _, err = m.Reply("1", first.ID, "Run another task"); err == nil {
		t.Fatal("concurrent conversation started")
	}
	if v, _ := m.Get("1", first.ID); v.Status != "done" {
		t.Fatal("prior conversation changed", v)
	}
}
