package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"netcheck/config"
	"netcheck/monitor"
)

type MockNotifier struct{}

func (m *MockNotifier) SendAlert(name, host string, port int, lastError string, duration time.Duration) error {
	return nil
}

func (m *MockNotifier) SendRecovery(name, host string, port int, downtime time.Duration) error {
	return nil
}

func (m *MockNotifier) SendTest(token, chatID string) error {
	return nil
}

func TestWebAPI(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "netcheck-web-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	cfgMgr, _ := config.NewManager(filepath.Join(tempDir, "config.json"))
	store, _ := monitor.NewStore(filepath.Join(tempDir, "targets.json"))
	notif := &MockNotifier{}
	checker := monitor.NewChecker(store, cfgMgr, notif)
	server := NewServer(cfgMgr, store, checker, notif)

	// Test 1: Add Target via POST /api/targets
	targetPayload := map[string]interface{}{
		"name":     "DNS Google",
		"host":     "8.8.8.8",
		"port":     53,
		"interval": 10,
		"timeout":  2,
	}
	body, _ := json.Marshal(targetPayload)
	req := httptest.NewRequest("POST", "/api/targets", bytes.NewReader(body))
	w := httptest.NewRecorder()
	server.handleTargets(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created monitor.Target
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Host != "8.8.8.8" || created.Port != 53 {
		t.Fatalf("Unexpected created target: %+v", created)
	}

	// Test 2: List Targets via GET /api/targets
	req = httptest.NewRequest("GET", "/api/targets", nil)
	w = httptest.NewRecorder()
	server.handleTargets(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", w.Code)
	}

	var list []monitor.Target
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("Expected 1 target, got %d", len(list))
	}

	// Test 3: Stats via GET /api/stats
	req = httptest.NewRequest("GET", "/api/stats", nil)
	w = httptest.NewRecorder()
	server.handleStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK on stats, got %d", w.Code)
	}
}
