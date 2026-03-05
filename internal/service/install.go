//go:build windows

// Package service implements Windows Service registration and control for
// WinClaw. The service runs under NT AUTHORITY\LocalService (a built-in
// low-privilege account) and uses machine-DPAPI to access the API key without
// touching the user's Credential Manager session.
package service

import (
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	// ServiceName is the short name used by the SCM.
	ServiceName = "WinClaw"

	// ServiceDisplayName appears in Services Management Console.
	ServiceDisplayName = "WinClaw AI Assistant"

	// ServiceDescription appears in the service properties dialog.
	ServiceDescription = "Windows-native AI assistant — background scheduler and prompt server. " +
		"Runs as LocalService; stores no credentials on disk."
)

// Install registers winclaw.exe as a Windows Service that starts automatically.
// exePath must be the absolute path to the binary. Requires administrator rights.
func Install(exePath string) error {
	if !filepath.IsAbs(exePath) {
		return fmt.Errorf("service install: binary path must be absolute, got %q", exePath)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service install: connect to SCM (run as administrator): %w", err)
	}
	defer m.Disconnect()

	// Fail clearly if already installed.
	if existing, err := m.OpenService(ServiceName); err == nil {
		existing.Close()
		return fmt.Errorf("service install: %q is already installed — run --uninstall-service first", ServiceName)
	}

	s, err := m.CreateService(ServiceName, exePath, mgr.Config{
		DisplayName:      ServiceDisplayName,
		Description:      ServiceDescription,
		StartType:        mgr.StartAutomatic,
		ServiceStartName: "NT AUTHORITY\\LocalService",
		// No password for a built-in account.
	})
	if err != nil {
		return fmt.Errorf("service install: CreateService: %w", err)
	}
	s.Close()
	return nil
}

// Uninstall removes the WinClaw Windows Service. The service must be stopped
// first (or this will attempt to stop it). Requires administrator rights.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service uninstall: connect to SCM (run as administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service uninstall: service not found: %w", err)
	}
	defer s.Close()

	// Attempt to stop first; ignore errors (service may already be stopped).
	q, _ := s.Query()
	if q.State == svc.Running || q.State == svc.StartPending {
		_, _ = s.Control(svc.Stop)
		// Wait briefly for stop.
		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			if q, err = s.Query(); err != nil || q.State == svc.Stopped {
				break
			}
		}
	}

	if err := s.Delete(); err != nil {
		return fmt.Errorf("service uninstall: delete: %w", err)
	}
	return nil
}

// Start sends a start request to the WinClaw service via SCM.
func Start() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service start: connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service start: open service (run --install-service first): %w", err)
	}
	defer s.Close()

	return s.Start()
}

// Stop sends a stop control request to the WinClaw service via SCM.
func Stop() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service stop: connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service stop: open service: %w", err)
	}
	defer s.Close()

	_, err = s.Control(svc.Stop)
	return err
}

// Status returns a human-readable string describing the service state:
// "running", "stopped", "starting", "stopping", or "not installed".
func Status() string {
	m, err := mgr.Connect()
	if err != nil {
		return "unknown (cannot connect to SCM)"
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return "not installed"
	}
	defer s.Close()

	q, err := s.Query()
	if err != nil {
		return "unknown"
	}
	switch q.State {
	case svc.Running:
		return "running"
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("state=%d", q.State)
	}
}
