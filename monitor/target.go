package monitor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Target struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Host                string     `json:"host"`
	Port                int        `json:"port"`
	Interval            int        `json:"interval"` // seconds
	Timeout             int        `json:"timeout"`  // seconds
	Enabled             bool       `json:"enabled"`
	Status              string     `json:"status"` // "UP", "DOWN", "PENDING"
	LatencyMs           int64      `json:"latency_ms"`
	LastError           string     `json:"last_error"`
	LastCheck           *time.Time `json:"last_check"`
	LastSuccess         *time.Time `json:"last_success"`
	LastFailure         *time.Time `json:"last_failure"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	WasUp               bool       `json:"was_up"`      // Whether it has been seen UP
	AlertSent           bool       `json:"alert_sent"`  // Whether down alert was already dispatched
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type Store struct {
	mu       sync.RWMutex
	filePath string
	targets  map[string]*Target
}

func NewStore(filePath string) (*Store, error) {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	s := &Store{
		filePath: filePath,
		targets:  make(map[string]*Target),
	}

	if _, err := os.Stat(filePath); err == nil {
		if err := s.loadLocked(); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *Store) loadLocked() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	var list []*Target
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, t := range list {
		if t.Status == "" {
			t.Status = "PENDING"
		}
		s.targets[t.ID] = t
	}
	return nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	list := make([]*Target, 0, len(s.targets))
	for _, t := range s.targets {
		list = append(list, t)
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

func (s *Store) GetAll() []*Target {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Target, 0, len(s.targets))
	for _, t := range s.targets {
		// make a copy
		tCopy := *t
		list = append(list, &tCopy)
	}
	return list
}

func (s *Store) Get(id string) (*Target, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.targets[id]
	if !ok {
		return nil, false
	}
	tCopy := *t
	return &tCopy, true
}

func (s *Store) Add(target *Target) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if target.Host == "" || target.Port <= 0 || target.Port > 65535 {
		return fmt.Errorf("invalid host or port")
	}
	if target.Interval <= 0 {
		target.Interval = 15
	}
	if target.Timeout <= 0 {
		target.Timeout = 5
	}
	target.ID = generateID()
	target.Enabled = true
	target.Status = "PENDING"
	target.WasUp = false
	target.AlertSent = false

	s.targets[target.ID] = target
	return s.saveLocked()
}

func (s *Store) Update(id string, updateFn func(t *Target)) (*Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.targets[id]
	if !ok {
		return nil, fmt.Errorf("target not found")
	}

	updateFn(t)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	tCopy := *t
	return &tCopy, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.targets[id]; !ok {
		return fmt.Errorf("target not found")
	}

	delete(s.targets, id)
	return s.saveLocked()
}
