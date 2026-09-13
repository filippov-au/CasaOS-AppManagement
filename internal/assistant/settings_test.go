package assistant

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestConnectionsSurviveSwitchRestartAndIndividualDisconnect(t *testing.T) {
	s := &SettingsStore{Root: t.TempDir()}
	first := Settings{Provider: Presets[0].ID, Model: Presets[0].Models[0], APIKey: "deepseek-private-key"}
	// Existing installations used the flat settings format.
	legacy, _ := json.Marshal(first)
	if err := os.WriteFile(s.path("1"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	second := Settings{Provider: Presets[1].ID, Model: Presets[1].Models[0], APIKey: "opencode-private-key"}
	if _, err := s.Save("1", second); err != nil {
		t.Fatal(err)
	}
	s = &SettingsStore{Root: s.Root}
	first.APIKey = ""
	if _, err := s.Save("1", first); err != nil {
		t.Fatal(err)
	}
	restored, err := s.Read("1")
	if err != nil || restored.APIKey != "deepseek-private-key" {
		t.Fatal("lost previous connection")
	}
	list, err := s.Connections("1")
	if err != nil || len(list) != 2 {
		t.Fatal("missing connections")
	}
	public, _ := json.Marshal(list)
	if strings.Contains(string(public), "private-key") || strings.Contains(string(public), "api_key") {
		t.Fatal("keys leaked")
	}
	if other, _ := s.Connections("2"); len(other) != 0 {
		t.Fatal("cross-user connection leak")
	}
	if err := s.Disconnect("1", "deepseek"); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Read("1")
	if current.Configured || current.APIKey != "" {
		t.Fatal("disconnected key retained")
	}
	remaining, _ := s.ReadProvider("1", "opencode-go")
	if remaining.APIKey != "opencode-private-key" {
		t.Fatal("other provider removed")
	}
	info, _ := os.Stat(s.path("1"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe file permissions")
	}
}
