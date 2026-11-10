package monitor

import (
	"context"
	"fmt"
	"net"
	"strconv"
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

	address := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	startTime := time.Now()

	conn, dialErr := net.DialTimeout("tcp", address, timeout)
	latency := time.Since(startTime)
	latencyMs := latency.Milliseconds()
	now := time.Now()

	cfg := c.cfgManager.Get()

	if dialErr == nil {
		_ = conn.Close()
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
		errMsg := dialErr.Error()
		var sendAlert bool

		_, _ = c.store.Update(id, func(t *Target) {
			t.LastCheck = &now
			t.Status = "DOWN"
			t.LatencyMs = 0
			t.LastError = errMsg
			t.ConsecutiveFailures++
			t.LastFailure = &now

			// Condition: "если соединение работало а потом перестало - отправляет сообщение в телеграмм"
			// WasUp indicates it worked before, and AlertSent ensures we only alert once per outage
			if t.WasUp && !t.AlertSent {
				sendAlert = true
				t.AlertSent = true
			}
		})

		logger.Error("Target [%s] %s is DOWN (check failed: %s)", target.Name, address, errMsg)

		if sendAlert {
			logger.Warn("ALERT TRIGGERED: Target [%s] %s was UP and is now DOWN! Sending Telegram notification...", target.Name, address)
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
