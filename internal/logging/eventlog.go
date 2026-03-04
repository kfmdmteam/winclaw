//go:build windows

package logging

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/svc/eventlog"
)

const sourceName = "WinClaw"

// EventLogger wraps a Windows Event Log handle and provides structured logging
// methods. If the event log is unavailable (e.g., insufficient permissions),
// all methods fall back to stderr so the application can continue operating.
type EventLogger struct {
	log      *eventlog.Log
	fallback bool // true when using stderr instead of the real event log
}

// Open opens (or creates) the WinClaw event log source. The source is
// registered under HKLM\SYSTEM\CurrentControlSet\Services\EventLog\Application\WinClaw.
// If registration or opening fails, Open returns an EventLogger that writes to
// stderr rather than propagating the error; callers should not treat a non-nil
// return as a guarantee that real event log output is active — check
// EventLogger.UsingFallback() if that distinction matters.
func Open() (*EventLogger, error) {
	// Attempt to install the event source if not already present. This requires
	// elevation on first run; subsequent runs succeed without it.
	_ = eventlog.InstallAsEventCreate(sourceName, eventlog.Error|eventlog.Warning|eventlog.Info)

	lg, err := eventlog.Open(sourceName)
	if err != nil {
		// Fall back silently — never crash just because event log is unavailable.
		fmt.Fprintf(os.Stderr, "[WinClaw] event log unavailable (%v), using stderr\n", err)
		return &EventLogger{fallback: true}, nil
	}
	return &EventLogger{log: lg}, nil
}

// UsingFallback reports whether the logger is writing to stderr rather than
// the Windows Event Log.
func (e *EventLogger) UsingFallback() bool {
	return e.fallback
}

// Info writes an informational event (Event ID 1).
func (e *EventLogger) Info(msg string) {
	if e.fallback {
		fmt.Fprintf(os.Stderr, "[INFO] %s\n", msg)
		return
	}
	if err := e.log.Info(1, msg); err != nil {
		fmt.Fprintf(os.Stderr, "[INFO] %s (event log error: %v)\n", msg, err)
	}
}

// Warning writes a warning event (Event ID 2).
func (e *EventLogger) Warning(msg string) {
	if e.fallback {
		fmt.Fprintf(os.Stderr, "[WARN] %s\n", msg)
		return
	}
	if err := e.log.Warning(2, msg); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] %s (event log error: %v)\n", msg, err)
	}
}

// Error writes an error event (Event ID 3).
func (e *EventLogger) Error(msg string) {
	if e.fallback {
		fmt.Fprintf(os.Stderr, "[ERROR] %s\n", msg)
		return
	}
	if err := e.log.Error(3, msg); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] %s (event log error: %v)\n", msg, err)
	}
}

// Security writes an audit event (Event ID 4) that records an action
// performed by subject, with optional additional detail. Security events are
// written at the Information level so they appear in the Application log
// without triggering alert filters designed for errors.
//
// Format: "SECURITY | action=<action> subject=<subject> detail=<detail>"
func (e *EventLogger) Security(action, subject, detail string) {
	msg := fmt.Sprintf("SECURITY | action=%s subject=%s detail=%s", action, subject, detail)
	if e.fallback {
		fmt.Fprintf(os.Stderr, "[SECURITY] %s\n", msg)
		return
	}
	if err := e.log.Info(4, msg); err != nil {
		fmt.Fprintf(os.Stderr, "[SECURITY] %s (event log error: %v)\n", msg, err)
	}
}

// Close releases the underlying event log handle. It is safe to call Close on
// a fallback logger (it is a no-op in that case).
func (e *EventLogger) Close() error {
	if e.fallback || e.log == nil {
		return nil
	}
	if err := e.log.Close(); err != nil {
		return fmt.Errorf("eventlog: close: %w", err)
	}
	return nil
}
