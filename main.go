package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"netcheck/config"
	"netcheck/logger"
	"netcheck/monitor"
	"netcheck/notifier"
	"netcheck/web"
)

func main() {
	portFlag := flag.Int("port", 0, "Web interface port (e.g. 8088)")
	dataDirFlag := flag.String("data", "./data", "Path to data directory")
	tgTokenFlag := flag.String("token", "", "Telegram Bot Token (optional override)")
	tgChatFlag := flag.String("chat", "", "Telegram Chat ID (optional override)")
	flag.Parse()

	dataDir := *dataDirFlag
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating data directory %s: %v\n", dataDir, err)
		os.Exit(1)
	}

	// Initialize Logger
	logPath := filepath.Join(dataDir, "netcheck.log")
	logInstance, err := logger.Init(logPath, 1000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing logger: %v\n", err)
		os.Exit(1)
	}
	defer logInstance.Close()

	logger.Info("==========================================")
	logger.Info("Starting NetCheck IP/Port Monitoring Service")
	logger.Info("Data directory: %s", dataDir)
	logger.Info("Log file: %s", logPath)
	logger.Info("==========================================")

	// Initialize Config
	cfgPath := filepath.Join(dataDir, "config.json")
	cfgManager, err := config.NewManager(cfgPath)
	if err != nil {
		logger.Error("Failed to initialize config: %v", err)
		os.Exit(1)
	}

	// Apply CLI flags if provided
	if *portFlag > 0 || *tgTokenFlag != "" || *tgChatFlag != "" {
		_ = cfgManager.Update(func(c *config.Config) {
			if *portFlag > 0 {
				c.WebPort = *portFlag
			}
			if *tgTokenFlag != "" {
				c.TelegramToken = *tgTokenFlag
			}
			if *tgChatFlag != "" {
				c.TelegramChatID = *tgChatFlag
			}
		})
	}

	// Initialize Notifier
	tgNotifier := notifier.NewTelegramNotifier(
		func() string { return cfgManager.Get().TelegramToken },
		func() string { return cfgManager.Get().TelegramChatID },
	)

	// Initialize Store
	targetsPath := filepath.Join(dataDir, "targets.json")
	store, err := monitor.NewStore(targetsPath)
	if err != nil {
		logger.Error("Failed to initialize targets store: %v", err)
		os.Exit(1)
	}
	logger.Info("Loaded %d target(s) from %s", len(store.GetAll()), targetsPath)

	// Initialize Checker
	checker := monitor.NewChecker(store, cfgManager, tgNotifier)
	checker.Start()

	// Initialize Web Server
	server := web.NewServer(cfgManager, store, checker, tgNotifier)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logger.Info("Received signal %s, shutting down...", sig)
		checker.Stop()
		_ = server.Stop()
		logger.Info("NetCheck shutdown complete.")
		os.Exit(0)
	}()

	// Start Web Server (blocks)
	if err := server.Start(); err != nil && err.Error() != "http: Server closed" {
		logger.Error("Server error: %v", err)
		os.Exit(1)
	}
}
