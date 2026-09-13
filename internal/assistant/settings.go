package assistant

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type SettingsStore struct {
	mu   sync.Mutex
	Root string
}

// The disk envelope never crosses the API or enters a model/session message.
// Embedding Settings keeps the existing single-provider file format readable.
type savedConnections struct {
	Settings
	Connections map[string]Settings `json:"connections,omitempty"`
}

var ownerPattern = regexp.MustCompile(`^[0-9]+$`)

func (s *SettingsStore) path(owner string) string { return filepath.Join(s.Root, owner+".json") }
func (s *SettingsStore) readAll(owner string) (savedConnections, error) {
	if !ownerPattern.MatchString(owner) {
		return savedConnections{}, errors.New("authenticated user required")
	}
	data, err := os.ReadFile(s.path(owner))
	saved := savedConnections{Settings: Settings{Provider: Presets[0].ID, Model: Presets[0].Models[0]}, Connections: map[string]Settings{}}
	if os.IsNotExist(err) {
		return saved, nil
	}
	if err != nil {
		return saved, err
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		return saved, err
	}
	if saved.Connections == nil {
		saved.Connections = map[string]Settings{}
	}
	if saved.APIKey != "" {
		saved.Connections[saved.Provider] = saved.Settings
	}
	saved.Configured = saved.APIKey != ""
	return saved, nil
}
func (s *SettingsStore) Read(owner string) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.readAll(owner)
	return saved.Settings, err
}
func (s *SettingsStore) ReadProvider(owner, provider string) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.readAll(owner)
	cfg := saved.Connections[provider]
	cfg.Configured = cfg.APIKey != ""
	return cfg, err
}
func (s *SettingsStore) Connections(owner string) ([]Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.readAll(owner)
	if err != nil {
		return nil, err
	}
	result := []Settings{}
	for _, p := range adapters {
		cfg, ok := saved.Connections[p.ID]
		if !ok || cfg.APIKey == "" {
			continue
		}
		cfg.APIKey = ""
		cfg.Configured = true
		result = append(result, cfg)
	}
	return result, nil
}
func (s *SettingsStore) write(owner string, saved savedConnections) error {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Root, ".settings-*")
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
	return os.Rename(f.Name(), s.path(owner))
}
func (s *SettingsStore) Save(owner string, cfg Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.readAll(owner)
	if err != nil {
		return Settings{}, err
	}
	if cfg.APIKey == "" {
		cfg.APIKey = saved.Connections[cfg.Provider].APIKey
	}
	if _, err = endpoint(cfg); err != nil {
		return Settings{}, err
	}
	if len(cfg.APIKey) > 4096 || strings.ContainsAny(cfg.APIKey, "\r\n") {
		return Settings{}, errors.New("invalid API key")
	}
	cfg.Configured = cfg.APIKey != ""
	saved.Settings = cfg
	if cfg.Configured {
		saved.Connections[cfg.Provider] = cfg
	}
	if err = s.write(owner, saved); err != nil {
		return Settings{}, err
	}
	cfg.APIKey = ""
	return cfg, nil
}
func (s *SettingsStore) Disconnect(owner, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.readAll(owner)
	if err != nil {
		return err
	}
	if provider != "deepseek" && provider != "opencode-go" {
		return errors.New("unknown provider")
	}
	delete(saved.Connections, provider)
	if saved.Provider == provider {
		saved.APIKey = ""
		saved.Configured = false
	}
	return s.write(owner, saved)
}
func (s *SettingsStore) Delete(owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ownerPattern.MatchString(owner) {
		return errors.New("authenticated user required")
	}
	err := os.Remove(s.path(owner))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
