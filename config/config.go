package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Config struct {
	WebPort                int    `json:"web_port"`
	TelegramToken          string `json:"telegram_token"`
	TelegramChatID         string `json:"telegram_chat_id"`
	NotifyRecovery         bool   `json:"notify_recovery"`
	FailedChecksThreshold  int    `json:"failed_checks_threshold"` // consecutive failed checks before Telegram alert
	DefaultInterval        int    `json:"default_interval"`        // in seconds
	DefaultTimeout         int    `json:"default_timeout"`         // in seconds
	DataDir                string `json:"data_dir"`
}

type Manager struct {
	mu       sync.RWMutex
	filePath string
	cfg      Config
}

func DefaultConfig() Config {
	return Config{
		WebPort:                8088,
		TelegramToken:          "",
		TelegramChatID:         "",
		NotifyRecovery:         true,
		FailedChecksThreshold:  2,
		DefaultInterval:        15,
		DefaultTimeout:         5,
		DataDir:                "./data",
	}
}

func NewManager(filePath string) (*Manager, error) {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	m := &Manager{
		filePath: filePath,
		cfg:      DefaultConfig(),
	}

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		if err := m.saveLocked(); err != nil {
			return nil, err
		}
	} else {
		if err := m.loadLocked(); err != nil {
			return nil, err
		}
	}

	return m, nil
}

func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) Update(fn func(c *Config)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.cfg)
	return m.saveLocked()
}

func (m *Manager) loadLocked() error {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	if c.WebPort == 0 {
		c.WebPort = 8088
	}
	if c.DefaultInterval == 0 {
		c.DefaultInterval = 15
	}
	if c.DefaultTimeout == 0 {
		c.DefaultTimeout = 5
	}
	if c.FailedChecksThreshold <= 0 {
		c.FailedChecksThreshold = 2
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	m.cfg = c
	return nil
}

func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := m.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, m.filePath)
}
