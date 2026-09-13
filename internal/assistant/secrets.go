package assistant

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var credentialArgument = regexp.MustCompile(`(?i)(password|passwd|api[_-]?key|token|secret|claim|username)`)
var credentialName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// AddSecret accepts a value through the authenticated UI, independently of chat.
// It is held in this session only; model messages contain references, never values.
func (m *Manager) AddSecret(owner, id, name, value string) (View, error) {
	if !credentialName.MatchString(name) || len(value) < 1 || len(value) > 4096 || strings.ContainsRune(value, 0) {
		return View{}, errors.New("use a credential name with lowercase letters, numbers or underscores, and a value of 1–4096 bytes")
	}
	s, err := m.get(owner, id)
	if err != nil {
		return View{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.view.Status != "done" {
		return View{}, ErrBusy
	}
	if s.secrets == nil {
		s.secrets = map[string]string{}
	}
	if _, exists := s.secrets[name]; !exists && len(s.secrets) >= 32 {
		return View{}, errors.New("credential limit reached for this conversation")
	}
	s.secrets[name] = value
	s.redactHistory()
	s.event("status", "Private credential added: "+name+". The AI can use $secret:"+name+" without seeing its value.", "")
	return s.snapshot(), s.checkpoint()
}
func (s *session) redact(text string) string {
	values := []string{s.cfg.APIKey}
	for _, v := range s.secrets {
		values = append(values, v)
	}
	// Short secret values still must not appear in observations sent to a provider.
	for _, v := range values {
		if v != "" && len(v) < 4 {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	return Redact(text, values...)
}
func (s *session) toolArguments(arguments string) (json.RawMessage, error) {
	var v interface{}
	if err := json.Unmarshal([]byte(arguments), &v); err != nil {
		return nil, err
	}
	var replace func(interface{}, string) (interface{}, error)
	replace = func(v interface{}, field string) (interface{}, error) {
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(x, "$secret:") {
				if !credentialArgument.MatchString(field) {
					return nil, errors.New("private credentials can only be used in password, API key, token, claim, or username fields")
				}
				name := strings.TrimPrefix(x, "$secret:")
				value, ok := s.secrets[name]
				if !ok {
					return nil, errors.New("private credential missing; ask the user to add " + name + " with the Add credential button")
				}
				return value, nil
			}
		case map[string]interface{}:
			for k, v := range x {
				out, err := replace(v, k)
				if err != nil {
					return nil, err
				}
				x[k] = out
			}
		case []interface{}:
			for i, v := range x {
				out, err := replace(v, field)
				if err != nil {
					return nil, err
				}
				x[i] = out
			}
		}
		return v, nil
	}
	out, err := replace(v, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}
