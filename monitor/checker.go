package monitor

import (
	"context"
	"fmt"
	"math"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"netcheck/config"
	"netcheck/logger"
	"netcheck/notifier"
)

type Checker struct {
	store      *Store
	cfgManager *config.Manager
	notifier   notifier.Notifier
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	inFlight   sync.Map
}

func NewChecker(store *Store, cfgManager *config.Manager, n notifier.Notifier) *Checker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Checker{
		store:      store,
		cfgManager: cfgManager,
		notifier:   n,
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (c *Checker) Start() {
	logger.Info("Starting monitor checker engine...")
	c.wg.Add(1)
	go c.loop()
}

func (c *Checker) Stop() {
	logger.Info("Stopping monitor checker engine...")
	c.cancel()
	c.wg.Wait()
	logger.Info("Monitor checker engine stopped.")
}

func (c *Checker) loop() {
	defer c.wg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Run initial check immediately
	c.checkAllDue()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.checkAllDue()
		}
	}
}

func (c *Checker) checkAllDue() {
	c.store.ReloadIfChanged()
	targets := c.store.GetAll()
	now := time.Now()

	for _, t := range targets {
		if !t.Enabled {
			continue
		}

		shouldCheck := false
		if t.LastCheck == nil {
			shouldCheck = true
		} else {
			interval := t.Interval
			if interval <= 0 {
				interval = 15
			}
			if now.Sub(*t.LastCheck) >= time.Duration(interval)*time.Second {
				shouldCheck = true
			}
		}

		if shouldCheck {
			go c.CheckTarget(t.ID)
		}
	}
}

func (c *Checker) CheckTarget(id string) {
	// Avoid overlapping checks for the same target
	if _, loaded := c.inFlight.LoadOrStore(id, true); loaded {
		return
	}
	defer c.inFlight.Delete(id)

	target, exists := c.store.Get(id)
	if !exists || !target.Enabled {
		return
	}

	timeoutSec := target.Timeout
	if timeoutSec <= 0 {
		timeoutSec = 5
	}
	timeout := time.Duration(timeoutSec) * time.Second

	var latencyMs int64
	var checkErr error
	var address string

	if target.Port > 0 {
		address = net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
		startTime := time.Now()
		conn, dialErr := net.DialTimeout("tcp", address, timeout)
		latency := time.Since(startTime)
		latencyMs = latency.Milliseconds()
		if dialErr != nil {
			checkErr = dialErr
		} else {
			_ = conn.Close()
		}
	} else {
		address = fmt.Sprintf("%s (ICMP Ping)", target.Host)
		lat, pingErr := pingHost(c.ctx, target.Host, timeout)
		latencyMs = lat
		checkErr = pingErr
	}

	now := time.Now()
	cfg := c.cfgManager.Get()

	if checkErr == nil {
		// SUCCESS
		var needRecoveryAlert bool
		var downtime time.Duration

		_, _ = c.store.Update(id, func(t *Target) {
			t.LastCheck = &now
			t.Status = "UP"
			t.LatencyMs = latencyMs
			t.LastError = ""
			t.ConsecutiveFailures = 0

			if t.AlertSent {
				needRecoveryAlert = true
				if t.LastFailure != nil {
					downtime = now.Sub(*t.LastFailure)
				}
				t.AlertSent = false
			}
			t.WasUp = true
			t.LastSuccess = &now
		})

		logger.Info("Target [%s] %s is UP (latency: %dms)", target.Name, address, latencyMs)

		if needRecoveryAlert && cfg.NotifyRecovery {
			logger.Info("Target [%s] %s recovered! Sending Telegram notification...", target.Name, address)
			if err := c.notifier.SendRecovery(target.Name, target.Host, target.Port, downtime); err != nil {
				logger.Error("Failed to send Telegram recovery alert for [%s] %s: %v", target.Name, address, err)
			} else {
				logger.Info("Telegram recovery notification sent for [%s] %s", target.Name, address)
			}
		}

	} else {
		// FAILURE
		errMsg := checkErr.Error()
		var sendAlert bool
		var consecutiveCount int

		threshold := cfg.FailedChecksThreshold
		if threshold <= 0 {
			threshold = 2
		}

		_, _ = c.store.Update(id, func(t *Target) {
			t.LastCheck = &now
			t.Status = "DOWN"
			t.LatencyMs = 0
			t.LastError = errMsg
			t.ConsecutiveFailures++
			t.LastFailure = &now
			consecutiveCount = t.ConsecutiveFailures

			// Condition: "если 2 проверки подряд были неудачны (после того как работало) - отправляет сообщение в телеграмм"
			// WasUp indicates it worked before, and AlertSent ensures we only alert once per outage
			if t.WasUp && !t.AlertSent && t.ConsecutiveFailures >= threshold {
				sendAlert = true
				t.AlertSent = true
			}
		})

		logger.Error("Target [%s] %s is DOWN (check failed: %s, failure %d/%d)", target.Name, address, errMsg, consecutiveCount, threshold)

		if sendAlert {
			logger.Warn("ALERT TRIGGERED: Target [%s] %s failed %d consecutive checks! Sending Telegram notification...", target.Name, address, consecutiveCount)
			if err := c.notifier.SendAlert(target.Name, target.Host, target.Port, errMsg, 0); err != nil {
				logger.Error("Failed to send Telegram alert for [%s] %s: %v", target.Name, address, err)
			} else {
				logger.Info("Telegram alert successfully sent for [%s] %s", target.Name, address)
			}
		}
	}
}

func (c *Checker) CheckNow(id string) error {
	target, exists := c.store.Get(id)
	if !exists {
		return fmt.Errorf("target not found")
	}
	go c.CheckTarget(target.ID)
	return nil
}

func pingHost(ctx context.Context, host string, timeout time.Duration) (int64, error) {
	timeoutSec := int(timeout.Seconds())
	if timeoutSec <= 0 {
		timeoutSec = 2
	}

	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		timeoutMs := timeoutSec * 1000
		cmd = exec.CommandContext(ctx, "ping", "-n", "1", "-w", strconv.Itoa(timeoutMs), host)
	} else if runtime.GOOS == "darwin" {
		cmd = exec.CommandContext(ctx, "ping", "-c", "1", "-t", strconv.Itoa(timeoutSec), host)
	} else {
		cmd = exec.CommandContext(ctx, "ping", "-c", "1", "-W", strconv.Itoa(timeoutSec), host)
	}

	out, err := cmd.CombinedOutput()
	outStr := string(out)

	if err != nil {
		trimmed := strings.TrimSpace(outStr)
		if trimmed != "" {
			lines := strings.Split(trimmed, "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				l := strings.TrimSpace(lines[i])
				if l != "" {
					return 0, fmt.Errorf("ping: %s", l)
				}
			}
		}
		return 0, fmt.Errorf("ping %s timed out or host unreachable", host)
	}

	// Always strictly parse latency from ping output - no fallback
	return parsePingLatency(outStr)
}

var pingTimeRegexes = []*regexp.Regexp{
	// 1. Linux/macOS/English Windows: "time=8.38 ms" or "time=14ms" or "time<1ms"
	regexp.MustCompile(`(?i)\btime[=<]\s*([0-9.]+)\s*ms`),
	// 2. Linux RTT summary: "rtt min/avg/max/mdev = 8.529/8.529/..."
	regexp.MustCompile(`rtt min/avg/max/mdev = [0-9.]+/([0-9.]+)/`),
	// 3. UTF-8 Russian/European Windows: "время=14мс", "время<1мс", "zeit=12ms", "temps=10ms"
	regexp.MustCompile(`(?i)(?:время|zeit|temps|tiempo)[=<]\s*([0-9.]+)\s*(?:ms|мс|msec|мсек)?`),
	// 4. Windows in ANY encoding (CP866, CP1251, UTF-8, etc.):
	// In Windows replies, time is always directly before TTL= (e.g. "... время=14мс TTL=118" or "... time<1ms TTL=118")
	regexp.MustCompile(`(?i)[=<]\s*([0-9.]+)[^0-9\r\n]*TTL[=\s]`),
	// 5. Windows summary: "Average = 14ms" or "Среднее = 14мс"
	regexp.MustCompile(`(?i)(?:average|среднее)[^0-9\r\n]*=\s*([0-9.]+)\s*(?:ms|мс|msec|мсек)?`),
	// 6. Generic fallback: "=14ms" or "=14мс" or "<1ms" or "<1мс"
	regexp.MustCompile(`[=<]\s*([0-9.]+)\s*(?:ms|мс|msec|мсек)`),
}

func parsePingLatency(output string) (int64, error) {
	for _, re := range pingTimeRegexes {
		matches := re.FindStringSubmatch(output)
		if len(matches) > 1 {
			val, err := strconv.ParseFloat(matches[1], 64)
			if err == nil {
				return int64(math.Round(val)), nil
			}
		}
	}
	return 0, fmt.Errorf("could not parse ping latency from utility output")
}
