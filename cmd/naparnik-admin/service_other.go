//go:build !windows

package main

import (
	"fmt"
	"log/slog"
)

func isWindowsService() bool { return false }

func runServiceCmd(_ string, _ *slog.Logger, _ []string) error {
	return fmt.Errorf("управление службой доступно только в Windows")
}

func runUnderSCM(_ string, _ *slog.Logger) error {
	return fmt.Errorf("запуск под диспетчером служб доступен только в Windows")
}
