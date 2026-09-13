package assistant

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSecretReferencesResolveOnlyForTools(t *testing.T) {
	s := &session{cfg: testSettings(), secrets: map[string]string{"indexer_key": "never-send-to-model"}}
	raw := `{"api_key":"$secret:indexer_key","tokens":["hello","$secret:indexer_key"]}`
	resolved, err := s.toolArguments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resolved), "never-send-to-model") {
		t.Fatal("missing secret substitution")
	}
	if strings.Contains(raw, "never-send-to-model") {
		t.Fatal("mutated model context")
	}
	text := s.redact("Use $secret:indexer_key. Output key: never-send-to-model")
	if !strings.Contains(text, "$secret:indexer_key") || strings.Contains(text, "never-send-to-model") {
		t.Fatal(text)
	}
	if _, err = s.toolArguments(`{"url":"$secret:indexer_key"}`); err == nil {
		t.Fatal("credential used as a destination")
	}
	if _, err = s.toolArguments(`{"api_key":"$secret:missing"}`); err == nil {
		t.Fatal("unknown secret accepted")
	}
}
func TestSecretEndpointIsOwnedAndNeverReturnsValue(t *testing.T) {
	m := NewManager(&testModel{tool: "inspect"}, &testRuntime{})
	v, err := m.Start("1", testSettings(), "configure app", "review")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, v.ID, "done")
	if _, err = m.AddSecret("2", v.ID, "key", "secret-value"); err != ErrNotFound {
		t.Fatal("cross-user secret")
	}
	view, err := m.AddSecret("1", v.ID, "key", "secret-value")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(view)
	if strings.Contains(string(b), "secret-value") {
		t.Fatal("secret returned")
	}
	if _, err = m.AddSecret("1", v.ID, "invalid name", "secret"); err == nil {
		t.Fatal("invalid secret name")
	}
}
