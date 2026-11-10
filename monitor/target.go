package monitor

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"netcheck/logger"
)

type Target struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Host                string     `json:"host"`
	Port                int        `json:"port"`
	Interval            int        `json:"interval"` // seconds
	Timeout             int        `json:"timeout"`  // seconds
	Enabled             bool       `json:"enabled"`
	Status              string     `json:"status"` // "UP", "DOWN", "PENDING", "DISABLED"
	LatencyMs           int64      `json:"latency_ms"`
	LastError           string     `json:"last_error"`
	LastCheck           *time.Time `json:"last_check"`
	LastSuccess         *time.Time `json:"last_success"`
	LastFailure         *time.Time `json:"last_failure"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	WasUp               bool       `json:"was_up"`     // Whether it has been seen UP
	AlertSent           bool       `json:"alert_sent"` // Whether down alert was already dispatched
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type Store struct {
	mu          sync.RWMutex
	filePath    string
	lastModTime time.Time
	targets     map[string]*Target
	order       []string // preserves order of targets in file
}

func NewStore(filePath string) (*Store, error) {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	s := &Store{
		filePath: filePath,
		targets:  make(map[string]*Target),
		order:    make([]string, 0),
	}

	// Migration: if targets.txt doesn't exist but targets.json exists, convert it
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		jsonPath := filepath.Join(dir, "targets.json")
		if _, jErr := os.Stat(jsonPath); jErr == nil {
			s.migrateFromJSON(jsonPath)
		}
	}

	if _, err := os.Stat(filePath); err == nil {
		if err := s.loadLocked(); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *Store) migrateFromJSON(jsonPath string) {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return
	}
	var list []*Target
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}

	for _, t := range list {
		if t.ID == "" {
			t.ID = generateID()
		}
		s.targets[t.ID] = t
		s.order = append(s.order, t.ID)
	}
	_ = s.saveLocked()
	_ = os.Remove(jsonPath)
	logger.Info("Migrated %d target(s) from targets.json to %s", len(list), s.filePath)
}

func (s *Store) ReloadIfChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.filePath)
	if err != nil {
		return
	}
	if fi.ModTime().After(s.lastModTime) {
		_ = s.loadLocked()
	}
}

func (s *Store) loadLocked() error {
	fi, err := os.Stat(s.filePath)
	if err != nil {
		return err
	}
	s.lastModTime = fi.ModTime()

	file, err := os.Open(s.filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Temporary map to retain runtime state of existing targets
	existingByKey := make(map[string]*Target)
	for _, t := range s.targets {
		key := fmt.Sprintf("%s:%d", strings.ToLower(t.Host), t.Port)
		existingByKey[key] = t
	}

	newTargets := make(map[string]*Target)
	newOrder := make([]string, 0)

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Format: "IP:PORT описание"
		var addrPart, descPart string
		idx := strings.IndexAny(line, " \t")
		if idx == -1 {
			addrPart = line
			descPart = ""
		} else {
			addrPart = strings.TrimSpace(line[:idx])
			descPart = strings.TrimSpace(line[idx+1:])
		}

		host, portStr, err := net.SplitHostPort(addrPart)
		if err != nil {
			lastColon := strings.LastIndex(addrPart, ":")
			if lastColon > 0 {
				host = addrPart[:lastColon]
				portStr = addrPart[lastColon+1:]
			} else {
				logger.Warn("Skipping invalid target at line %d: %s", lineNum, line)
				continue
			}
		}

		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			logger.Warn("Invalid port at line %d in %s: %s", lineNum, s.filePath, line)
			continue
		}

		key := fmt.Sprintf("%s:%d", strings.ToLower(host), port)
		target, found := existingByKey[key]
		if found {
			// Update description if changed in file
			if descPart != "" {
				target.Name = descPart
			}
		} else {
			name := descPart
			if name == "" {
				name = fmt.Sprintf("%s:%d", host, port)
			}
			target = &Target{
				ID:        generateID(),
				Name:      name,
				Host:      host,
				Port:      port,
				Interval:  15,
				Timeout:   5,
				Enabled:   true,
				Status:    "PENDING",
				WasUp:     false,
				AlertSent: false,
			}
		}

		newTargets[target.ID] = target
		newOrder = append(newOrder, target.ID)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	s.targets = newTargets
	s.order = newOrder
	return nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	var buf bytes.Buffer
	buf.WriteString("# NetCheck Targets\n")
	buf.WriteString("# Формат: IP:PORT Описание\n\n")

	for _, id := range s.order {
		t, ok := s.targets[id]
		if !ok {
			continue
		}
		desc := strings.TrimSpace(t.Name)
		defaultName := fmt.Sprintf("%s:%d", t.Host, t.Port)

		if desc != "" && desc != defaultName {
			buf.WriteString(fmt.Sprintf("%s:%d %s\n", t.Host, t.Port, desc))
		} else {
			buf.WriteString(fmt.Sprintf("%s:%d\n", t.Host, t.Port))
		}
	}

	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.filePath); err != nil {
		return err
	}

	fi, err := os.Stat(s.filePath)
	if err == nil {
		s.lastModTime = fi.ModTime()
	}
	return nil
}

func (s *Store) GetAll() []*Target {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Target, 0, len(s.order))
	for _, id := range s.order {
		if t, ok := s.targets[id]; ok {
			tCopy := *t
			list = append(list, &tCopy)
		}
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

	target.Host = strings.TrimSpace(target.Host)
	target.Name = strings.TrimSpace(target.Name)

	if target.Host == "" || target.Port <= 0 || target.Port > 65535 {
		return fmt.Errorf("invalid host or port")
	}

	// Check if already exists
	key := fmt.Sprintf("%s:%d", strings.ToLower(target.Host), target.Port)
	for _, existing := range s.targets {
		if fmt.Sprintf("%s:%d", strings.ToLower(existing.Host), existing.Port) == key {
			return fmt.Errorf("target %s:%d already exists", target.Host, target.Port)
		}
	}

	if target.Interval <= 0 {
		target.Interval = 15
	}
	if target.Timeout <= 0 {
		target.Timeout = 5
	}
	if target.Name == "" {
		target.Name = fmt.Sprintf("%s:%d", target.Host, target.Port)
	}

	target.ID = generateID()
	target.Enabled = true
	target.Status = "PENDING"
	target.WasUp = false
	target.AlertSent = false

	s.targets[target.ID] = target
	s.order = append(s.order, target.ID)
	return s.saveLocked()
}

func (s *Store) Update(id string, updateFn func(t *Target)) (*Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.targets[id]
	if !ok {
		return nil, fmt.Errorf("target not found")
	}

	oldHost := t.Host
	oldPort := t.Port
	oldName := t.Name

	updateFn(t)

	// Only rewrite targets.txt if host, port, or name changed
	if t.Host != oldHost || t.Port != oldPort || t.Name != oldName {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
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
	newOrder := make([]string, 0, len(s.order)-1)
	for _, itemID := range s.order {
		if itemID != id {
			newOrder = append(newOrder, itemID)
		}
	}
	s.order = newOrder

	return s.saveLocked()
}

func (s *Store) FilePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.filePath
}

func (s *Store) ReadRawFile() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}
