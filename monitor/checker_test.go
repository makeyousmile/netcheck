package monitor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"netcheck/config"
)

type MockNotifier struct {
	mu           sync.Mutex
	alertCount   int
	recoverCount int
	lastAlert    string
	lastError    string
}

func (m *MockNotifier) SendAlert(name, host string, port int, lastError string, duration time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alertCount++
	m.lastAlert = fmt.Sprintf("%s:%d", host, port)
	m.lastError = lastError
	return nil
}

func (m *MockNotifier) SendRecovery(name, host string, port int, downtime time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recoverCount++
	return nil
}

func (m *MockNotifier) SendTest(token, chatID string) error {
	return nil
}

func TestCheckerTransitionAndTelegramAlert(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "netcheck-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	cfgPath := filepath.Join(tempDir, "config.json")
	cfgMgr, err := config.NewManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	targetsPath := filepath.Join(tempDir, "targets.json")
	store, err := NewStore(targetsPath)
	if err != nil {
		t.Fatal(err)
	}

	// Start a dummy TCP server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpPort := ln.Addr().(*net.TCPAddr).Port

	// Accept connections in background
	stopAccept := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	mockNotif := &MockNotifier{}
	checker := NewChecker(store, cfgMgr, mockNotif)

	// 1. Add target
	target := &Target{
		Name:     "Local Test Service",
		Host:     "127.0.0.1",
		Port:     tcpPort,
		Interval: 1,
		Timeout:  1,
	}
	if err := store.Add(target); err != nil {
		t.Fatal(err)
	}

	// 2. Perform check while TCP server is running
	checker.CheckTarget(target.ID)

	updated, ok := store.Get(target.ID)
	if !ok || updated.Status != "UP" {
		t.Fatalf("Expected target status UP, got %v", updated.Status)
	}
	if !updated.WasUp {
		t.Fatalf("Expected WasUp to be true")
	}

	// No alerts should have fired yet
	mockNotif.mu.Lock()
	if mockNotif.alertCount != 0 {
		t.Fatalf("Expected 0 alerts, got %d", mockNotif.alertCount)
	}
	mockNotif.mu.Unlock()

	// 3. Now SHUT DOWN the TCP server (connection was working and now stopped!)
	close(stopAccept)
	_ = ln.Close()
	time.Sleep(50 * time.Millisecond)

	// 4. Perform check again
	checker.CheckTarget(target.ID)

	updated, ok = store.Get(target.ID)
	if !ok || updated.Status != "DOWN" {
		t.Fatalf("Expected target status DOWN, got %v", updated.Status)
	}

	// The alert must have triggered!
	mockNotif.mu.Lock()
	if mockNotif.alertCount != 1 {
		t.Fatalf("Expected exactly 1 alert, got %d", mockNotif.alertCount)
	}
	mockNotif.mu.Unlock()

	// 5. Check again while still DOWN — it should NOT repeat alert spam
	checker.CheckTarget(target.ID)
	mockNotif.mu.Lock()
	if mockNotif.alertCount != 1 {
		t.Fatalf("Expected still 1 alert (no spam), got %d", mockNotif.alertCount)
	}
	mockNotif.mu.Unlock()

	// 6. Restart TCP server on the SAME port
	ln2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", tcpPort))
	if err != nil {
		t.Skipf("Port %d busy or could not re-listen: %v", tcpPort, err)
		return
	}
	defer ln2.Close()
	go func() {
		for {
			conn, err := ln2.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// 7. Perform check again — service is back UP!
	checker.CheckTarget(target.ID)

	updated, _ = store.Get(target.ID)
	if updated.Status != "UP" {
		t.Fatalf("Expected status UP after recovery, got %v", updated.Status)
	}

	mockNotif.mu.Lock()
	if mockNotif.recoverCount != 1 {
		t.Fatalf("Expected 1 recovery alert, got %d", mockNotif.recoverCount)
	}
	mockNotif.mu.Unlock()
}
