package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Level string

const (
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
	LevelDebug Level = "DEBUG"
)

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     Level  `json:"level"`
	Message   string `json:"message"`
}

type Logger struct {
	mu       sync.RWMutex
	file     *os.File
	filePath string
	entries  []LogEntry
	maxSize  int
}

var globalLogger *Logger

func Init(filePath string, maxEntries int) (*Logger, error) {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}

	if maxEntries <= 0 {
		maxEntries = 500
	}

	l := &Logger{
		file:     f,
		filePath: filePath,
		entries:  make([]LogEntry, 0, maxEntries),
		maxSize:  maxEntries,
	}
	globalLogger = l
	return l, nil
}

func (l *Logger) log(level Level, format string, args ...interface{}) {
	now := time.Now()
	msg := fmt.Sprintf(format, args...)
	entry := LogEntry{
		Timestamp: now.Format("2006-01-02 15:04:05"),
		Level:     level,
		Message:   msg,
	}

	formatted := fmt.Sprintf("[%s] [%-5s] %s\n", entry.Timestamp, entry.Level, entry.Message)

	// Output to stdout
	os.Stdout.WriteString(formatted)

	l.mu.Lock()
	defer l.mu.Unlock()

	// Output to file
	if l.file != nil {
		l.file.WriteString(formatted)
	}

	// In-memory buffer for web UI
	if len(l.entries) >= l.maxSize {
		l.entries = l.entries[1:]
	}
	l.entries = append(l.entries, entry)
}

func (l *Logger) Info(format string, args ...interface{}) {
	l.log(LevelInfo, format, args...)
}

func (l *Logger) Warn(format string, args ...interface{}) {
	l.log(LevelWarn, format, args...)
}

func (l *Logger) Error(format string, args ...interface{}) {
	l.log(LevelError, format, args...)
}

func (l *Logger) Debug(format string, args ...interface{}) {
	l.log(LevelDebug, format, args...)
}

func (l *Logger) GetEntries() []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	res := make([]LogEntry, len(l.entries))
	copy(res, l.entries)
	return res
}

func (l *Logger) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make([]LogEntry, 0, l.maxSize)
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

func (l *Logger) FilePath() string {
	return l.filePath
}

// Global convenience wrappers
func Info(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.Info(format, args...)
	} else {
		fmt.Printf("[INFO] "+format+"\n", args...)
	}
}

func Warn(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.Warn(format, args...)
	} else {
		fmt.Printf("[WARN] "+format+"\n", args...)
	}
}

func Error(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.Error(format, args...)
	} else {
		fmt.Printf("[ERROR] "+format+"\n", args...)
	}
}

func Debug(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.Debug(format, args...)
	} else {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

func GetEntries() []LogEntry {
	if globalLogger != nil {
		return globalLogger.GetEntries()
	}
	return nil
}

func Clear() {
	if globalLogger != nil {
		globalLogger.Clear()
	}
}
