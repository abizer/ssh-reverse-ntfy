package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/abizer/nssh/v2/internal/wire"
)

// LogEvent is the on-disk schema for each JSONL log line. Both the writer
// (logEvent below) and the reader (status.go::formatEvent) marshal against
// this type, so renaming or adding a field is type-checked instead of
// grep-and-pray. Exit is a pointer so callers can record an explicit zero
// (exit=0, success) without omitempty dropping the field.
type LogEvent struct {
	TS    string `json:"ts"`
	Event string `json:"event"`
	Side  string `json:"side,omitempty"`
	PID   int    `json:"pid,omitempty"`

	// Wire-message details (msg-send / msg-recv).
	Kind string `json:"kind,omitempty"`
	Mime string `json:"mime,omitempty"`
	ID   string `json:"id,omitempty"`
	URL  string `json:"url,omitempty"`
	Size int    `json:"size,omitempty"`

	// Session lifecycle.
	Target    string `json:"target,omitempty"`
	Host      string `json:"host,omitempty"`
	Server    string `json:"server,omitempty"`
	Topic     string `json:"topic,omitempty"`
	Version   string `json:"version,omitempty"`
	Exit      *int   `json:"exit,omitempty"`
	Transport string `json:"transport,omitempty"`
	Joined    int    `json:"joined,omitempty"`

	// Shim invocation.
	Persona string   `json:"persona,omitempty"`
	Args    []string `json:"args,omitempty"`

	// Subscriber resilience (subscribe-up / subscribe-down).
	Reconnect bool   `json:"reconnect,omitempty"`
	Gap       string `json:"gap,omitempty"`
	Since     string `json:"since,omitempty"`

	// Error context.
	Err string `json:"err,omitempty"`
}

var (
	logFile *os.File
	logMu   sync.Mutex
	logSide string // "session" (local) or persona name (remote shim)
)

// openLog opens the per-topic JSONL log file for appending. Silently no-ops
// if NSSH_LOG=0 or the file can't be opened — logging is best-effort.
func openLog(topic, side string) {
	if os.Getenv("NSSH_LOG") == "0" {
		return
	}
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "nssh."+topic+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	logFile = f
	logSide = side
}

// logEvent writes one JSONL line, stamping ts/side/pid. Safe to call before
// openLog (no-op). Line writes are atomic under POSIX O_APPEND for size <
// PIPE_BUF (~4KB), so concurrent shim invocations on the same log don't
// interleave.
func logEvent(e LogEvent) {
	if logFile == nil {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	e.Side = logSide
	e.PID = os.Getpid()
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	logFile.Write(append(data, '\n'))
}

// logMessage emits a msg-send (dir=="out") or msg-recv (otherwise) event
// with the wire envelope details. size is the payload in bytes — attachment
// size for images, decoded body length for inline text, 0 if unknown.
func logMessage(dir string, env wire.Envelope, size int) {
	event := "msg-recv"
	if dir == "out" {
		event = "msg-send"
	}
	logEvent(LogEvent{
		Event: event,
		Kind:  env.Kind,
		Mime:  env.Mime,
		ID:    env.ID,
		URL:   env.URL,
		Size:  size,
	})
}
