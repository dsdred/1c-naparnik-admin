//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"naparnikadmin/internal/config"
)

const (
	serviceName  = "naparnik-admin"
	serviceTitle = "1С:Напарник — шлюз и админка"
)

// isWindowsService сообщает, запущен ли процесс службой Windows.
func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runServiceCmd обслуживает команды управления службой.
func runServiceCmd(configPath string, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("укажите действие: install | remove | start | stop | status")
	}
	action := strings.ToLower(args[0])
	switch action {
	case "install":
		return installService(configPath)
	case "remove", "uninstall":
		return removeService()
	case "start":
		return startService()
	case "stop":
		return stopService()
	case "status":
		return statusService()
	default:
		return fmt.Errorf("неизвестное действие %q (install|remove|start|stop|status)", action)
	}
}

// installService регистрирует службу с автозапуском.
func installService(configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	cfgAbs, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfgAbs); err != nil {
		return fmt.Errorf("файл конфигурации не найден: %s", cfgAbs)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб (нужны права администратора): %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("служба %q уже установлена (удалите её командой service remove)", serviceName)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: serviceTitle,
		Description: "Шлюз к публичному API «1С:Напарник разработчика» и админка. " +
			"Поддерживает OpenAI, Anthropic и Gemini протоколы.",
		StartType: mgr.StartAutomatic,
	}, "-config", cfgAbs, "serve")
	if err != nil {
		return err
	}
	defer s.Close()

	fmt.Printf("служба %q установлена\n  исполняемый файл: %s\n  конфигурация: %s\n  автозапуск: да\n",
		serviceName, exe, cfgAbs)
	fmt.Printf("запустить: naparnik-admin service start\n")
	return nil
}

func removeService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб (нужны права администратора): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("служба %q не установлена", serviceName)
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Printf("служба %q удалена\n", serviceName)
	return nil
}

func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб (нужна консоль от имени администратора): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("служба %q не установлена", serviceName)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("запуск: %w", err)
	}
	fmt.Printf("служба %q запускается…\n", serviceName)
	return waitState(s, svc.Running, 20*time.Second)
}

func stopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб (нужна консоль от имени администратора): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("служба %q не установлена", serviceName)
	}
	defer s.Close()

	status, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("остановка: %w", err)
	}
	fmt.Printf("служба %q останавливается (была в состоянии %s)…\n", serviceName, stateName(status.State))
	return waitState(s, svc.Stopped, 20*time.Second)
}

func statusService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб (нужна консоль от имени администратора): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("служба %q не установлена", serviceName)
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return err
	}
	fmt.Printf("служба %q: %s\n", serviceName, stateName(status.State))
	return nil
}

func waitState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == want {
			fmt.Printf("состояние: %s\n", stateName(status.State))
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("служба не перешла в состояние %s за %s", stateName(want), timeout)
}

func stateName(st svc.State) string {
	switch st {
	case svc.Stopped:
		return "остановлена"
	case svc.StartPending:
		return "запускается"
	case svc.StopPending:
		return "останавливается"
	case svc.Running:
		return "работает"
	case svc.Paused:
		return "приостановлена"
	}
	return fmt.Sprintf("неизвестно (%d)", st)
}

// runUnderSCM выполняет сервис под управлением диспетчера служб.
func runUnderSCM(configPath string, log *slog.Logger) error {
	cfgAbs, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	// Служба стартует из C:\Windows\System32, поэтому рабочую папку
	// переносим к конфигурации: относительный data_dir должен разрешаться рядом с ней.
	if err := os.Chdir(filepath.Dir(cfgAbs)); err != nil {
		log.Warn("не удалось сменить рабочую папку", "err", err)
	}
	cfg, err := config.Load(cfgAbs)
	if err != nil {
		return err
	}
	// Под SCM stdout/stderr не сохраняются, поэтому журнал пишем в файл рядом с базой.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err == nil {
		logPath := filepath.Join(cfg.DataDir, "service.log")
		if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
			log = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
			log.Info("журнал службы", "path", logPath)
		}
	}
	return svc.Run(serviceName, &scmHandler{cfg: cfg, log: log})
}

type scmHandler struct {
	cfg *config.Config
	log *slog.Logger
}

func (h *scmHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveWithContext(ctx, h.cfg, h.log) }()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			if err != nil {
				h.log.Error("служба остановилась с ошибкой", "err", err)
				return true, 1
			}
			return false, 0
		case c := <-requests:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}
