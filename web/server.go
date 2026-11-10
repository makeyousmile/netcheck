package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"netcheck/config"
	"netcheck/logger"
	"netcheck/monitor"
	"netcheck/notifier"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	cfgManager *config.Manager
	store      *monitor.Store
	checker    *monitor.Checker
	notifier   notifier.Notifier
	server     *http.Server
}

func NewServer(cfgManager *config.Manager, store *monitor.Store, checker *monitor.Checker, n notifier.Notifier) *Server {
	return &Server{
		cfgManager: cfgManager,
		store:      store,
		checker:    checker,
		notifier:   n,
	}
}

func (s *Server) Start() error {
	cfg := s.cfgManager.Get()
	addr := fmt.Sprintf(":%d", cfg.WebPort)

	mux := http.NewServeMux()

	// Static files
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(staticContent)))

	// API routes
	mux.HandleFunc("/api/targets", s.handleTargets)
	mux.HandleFunc("/api/targets/", s.handleTargetByID)
	mux.HandleFunc("/api/targets-file", s.handleTargetsFile)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/test-telegram", s.handleTestTelegram)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/stats", s.handleStats)

	s.server = &http.Server{
		Addr:         addr,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	logger.Info("Web interface listening on http://0.0.0.0:%d", cfg.WebPort)
	return s.server.ListenAndServe()
}

func (s *Server) Stop() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonResponse(w, status, map[string]string{"error": msg})
}

// Handlers

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		targets := s.store.GetAll()
		jsonResponse(w, http.StatusOK, targets)

	case http.MethodPost:
		var req struct {
			Name     string `json:"name"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Interval int    `json:"interval"`
			Timeout  int    `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}

		req.Host = strings.TrimSpace(req.Host)
		req.Name = strings.TrimSpace(req.Name)

		if req.Host == "" {
			jsonError(w, http.StatusBadRequest, "Host/IP is required")
			return
		}
		if req.Port < 0 || req.Port > 65535 {
			jsonError(w, http.StatusBadRequest, "Port must be between 0 and 65535")
			return
		}

		cfg := s.cfgManager.Get()
		if req.Interval <= 0 {
			req.Interval = cfg.DefaultInterval
		}
		if req.Timeout <= 0 {
			req.Timeout = cfg.DefaultTimeout
		}
		if req.Name == "" {
			if req.Port > 0 {
				req.Name = fmt.Sprintf("%s:%d", req.Host, req.Port)
			} else {
				req.Name = req.Host
			}
		}

		target := &monitor.Target{
			Name:     req.Name,
			Host:     req.Host,
			Port:     req.Port,
			Interval: req.Interval,
			Timeout:  req.Timeout,
		}

		if err := s.store.Add(target); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		if target.Port > 0 {
			logger.Info("Added new target [%s] %s:%d (Interval: %ds, Timeout: %ds)", target.Name, target.Host, target.Port, target.Interval, target.Timeout)
		} else {
			logger.Info("Added new target [%s] %s (ICMP Ping, Interval: %ds, Timeout: %ds)", target.Name, target.Host, target.Interval, target.Timeout)
		}
		go s.checker.CheckTarget(target.ID)
		jsonResponse(w, http.StatusCreated, target)

	default:
		jsonError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *Server) handleTargetByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/targets/")
	parts := strings.Split(path, "/")
	id := parts[0]

	if id == "" {
		jsonError(w, http.StatusBadRequest, "Target ID is required")
		return
	}

	// Sub-actions: /api/targets/{id}/check, /api/targets/{id}/toggle
	if len(parts) > 1 {
		action := parts[1]
		switch action {
		case "check":
			if r.Method != http.MethodPost {
				jsonError(w, http.StatusMethodNotAllowed, "POST required")
				return
			}
			if err := s.checker.CheckNow(id); err != nil {
				jsonError(w, http.StatusNotFound, err.Error())
				return
			}
			jsonResponse(w, http.StatusOK, map[string]string{"status": "check initiated"})
			return

		case "toggle":
			if r.Method != http.MethodPost {
				jsonError(w, http.StatusMethodNotAllowed, "POST required")
				return
			}
			target, err := s.store.Update(id, func(t *monitor.Target) {
				t.Enabled = !t.Enabled
				if !t.Enabled {
					t.Status = "DISABLED"
				} else {
					t.Status = "PENDING"
				}
			})
			if err != nil {
				jsonError(w, http.StatusNotFound, err.Error())
				return
			}
			logger.Info("Target [%s] %s:%d enabled state toggled to %v", target.Name, target.Host, target.Port, target.Enabled)
			if target.Enabled {
				go s.checker.CheckTarget(id)
			}
			jsonResponse(w, http.StatusOK, target)
			return

		default:
			jsonError(w, http.StatusNotFound, "Action not found")
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		target, ok := s.store.Get(id)
		if !ok {
			jsonError(w, http.StatusNotFound, "Target not found")
			return
		}
		jsonResponse(w, http.StatusOK, target)

	case http.MethodPut:
		var req struct {
			Name     string `json:"name"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Interval int    `json:"interval"`
			Timeout  int    `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}
		req.Host = strings.TrimSpace(req.Host)
		req.Name = strings.TrimSpace(req.Name)

		if req.Host == "" {
			jsonError(w, http.StatusBadRequest, "Host/IP is required")
			return
		}
		if req.Port < 0 || req.Port > 65535 {
			jsonError(w, http.StatusBadRequest, "Port must be between 0 and 65535")
			return
		}
		if req.Name == "" {
			if req.Port > 0 {
				req.Name = fmt.Sprintf("%s:%d", req.Host, req.Port)
			} else {
				req.Name = req.Host
			}
		}
		if req.Interval <= 0 {
			req.Interval = 15
		}
		if req.Timeout <= 0 {
			req.Timeout = 5
		}

		target, err := s.store.Update(id, func(t *monitor.Target) {
			t.Name = req.Name
			t.Host = req.Host
			t.Port = req.Port
			t.Interval = req.Interval
			t.Timeout = req.Timeout
		})
		if err != nil {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		logger.Info("Updated target [%s] %s:%d", target.Name, target.Host, target.Port)
		go s.checker.CheckTarget(id)
		jsonResponse(w, http.StatusOK, target)

	case http.MethodDelete:
		target, ok := s.store.Get(id)
		if !ok {
			jsonError(w, http.StatusNotFound, "Target not found")
			return
		}
		if err := s.store.Delete(id); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		logger.Info("Deleted target [%s] %s:%d", target.Name, target.Host, target.Port)
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		jsonError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.cfgManager.Get()
		maskedToken := ""
		if len(cfg.TelegramToken) > 8 {
			maskedToken = cfg.TelegramToken[:4] + "..." + cfg.TelegramToken[len(cfg.TelegramToken)-4:]
		} else if cfg.TelegramToken != "" {
			maskedToken = "******"
		}
		resp := map[string]interface{}{
			"telegram_token_set": cfg.TelegramToken != "",
			"telegram_token":     cfg.TelegramToken,
			"telegram_masked":    maskedToken,
			"telegram_chat_id":   cfg.TelegramChatID,
			"notify_recovery":    cfg.NotifyRecovery,
			"default_interval":   cfg.DefaultInterval,
			"default_timeout":    cfg.DefaultTimeout,
			"web_port":           cfg.WebPort,
		}
		jsonResponse(w, http.StatusOK, resp)

	case http.MethodPost:
		var req struct {
			TelegramToken   string `json:"telegram_token"`
			TelegramChatID  string `json:"telegram_chat_id"`
			NotifyRecovery  *bool  `json:"notify_recovery"`
			DefaultInterval int    `json:"default_interval"`
			DefaultTimeout  int    `json:"default_timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}

		err := s.cfgManager.Update(func(c *config.Config) {
			if strings.TrimSpace(req.TelegramToken) != "" {
				c.TelegramToken = strings.TrimSpace(req.TelegramToken)
			}
			c.TelegramChatID = strings.TrimSpace(req.TelegramChatID)
			if req.NotifyRecovery != nil {
				c.NotifyRecovery = *req.NotifyRecovery
			}
			if req.DefaultInterval > 0 {
				c.DefaultInterval = req.DefaultInterval
			}
			if req.DefaultTimeout > 0 {
				c.DefaultTimeout = req.DefaultTimeout
			}
		})

		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		logger.Info("Settings updated successfully")
		jsonResponse(w, http.StatusOK, map[string]string{"status": "settings updated"})

	default:
		jsonError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req struct {
		TelegramToken  string `json:"telegram_token"`
		TelegramChatID string `json:"telegram_chat_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	cfg := s.cfgManager.Get()
	token := req.TelegramToken
	if token == "" {
		token = cfg.TelegramToken
	}
	chatID := req.TelegramChatID
	if chatID == "" {
		chatID = cfg.TelegramChatID
	}

	if token == "" || chatID == "" {
		jsonError(w, http.StatusBadRequest, "Telegram Bot Token and Chat ID must be specified")
		return
	}

	err := s.notifier.SendTest(token, chatID)
	if err != nil {
		logger.Error("Failed to send test Telegram message: %v", err)
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("Telegram error: %s", err.Error()))
		return
	}

	logger.Info("Test Telegram message sent successfully to chat %s", chatID)
	jsonResponse(w, http.StatusOK, map[string]string{"status": "Test notification sent successfully!"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		entries := logger.GetEntries()
		jsonResponse(w, http.StatusOK, entries)

	case http.MethodDelete:
		logger.Clear()
		logger.Info("Logs cleared by user")
		jsonResponse(w, http.StatusOK, map[string]string{"status": "logs cleared"})

	default:
		jsonError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}

	targets := s.store.GetAll()
	total := len(targets)
	upCount := 0
	downCount := 0
	pendingCount := 0
	disabledCount := 0

	for _, t := range targets {
		if !t.Enabled {
			disabledCount++
		} else {
			switch t.Status {
			case "UP":
				upCount++
			case "DOWN":
				downCount++
			default:
				pendingCount++
			}
		}
	}

	stats := map[string]int{
		"total":    total,
		"up":       upCount,
		"down":     downCount,
		"pending":  pendingCount,
		"disabled": disabledCount,
	}

	jsonResponse(w, http.StatusOK, stats)
}

func (s *Server) handleTargetsFile(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		content, err := s.store.ReadRawFile()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{
			"path":    s.store.FilePath(),
			"content": content,
		})

	case http.MethodPost:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}

		if err := os.WriteFile(s.store.FilePath(), []byte(req.Content), 0644); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}

		s.store.ReloadIfChanged()
		logger.Info("targets.txt updated via Web UI (%d targets loaded)", len(s.store.GetAll()))
		jsonResponse(w, http.StatusOK, map[string]string{"status": "targets file updated"})

	default:
		jsonError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func ParseIntParam(r *http.Request, key string, fallback int) int {
	valStr := r.URL.Query().Get(key)
	if valStr == "" {
		return fallback
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return fallback
	}
	return val
}
