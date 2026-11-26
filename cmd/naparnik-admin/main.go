// Команда naparnik-admin — шлюз и админка «1С:Напарника разработчика».
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"naparnikadmin/internal/app"
	"naparnikadmin/internal/config"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "путь к файлу конфигурации")
	flag.Usage = usage
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	args := flag.Args()
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(*configPath, log)
	case "key":
		err = runKey(*configPath, log, args[1:])
	case "token":
		err = runToken(*configPath, log, args[1:])
	case "doctor":
		err = runDoctor(*configPath, log)
	case "service":
		err = runServiceCmd(*configPath, log, args[1:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("неизвестная команда %q", cmd)
	}
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `naparnik-admin — шлюз к публичному API «1С:Напарник разработчика».

Использование:
  naparnik-admin [-config config.yaml] serve            запустить сервис
  naparnik-admin [-config config.yaml] key add -name N -key K
  naparnik-admin [-config config.yaml] key list
  naparnik-admin [-config config.yaml] token create -key-id 1 -name "Cline"
  naparnik-admin [-config config.yaml] token list
  naparnik-admin [-config config.yaml] doctor           проверить конфигурацию и базу
  naparnik-admin [-config config.yaml] service install  установить службу Windows
  naparnik-admin service start|stop|status|remove        управление службой

Переменные окружения:
  NAPARNIK_ADMIN_DATA_DIR     каталог данных (по умолчанию ./data)
  NAPARNIK_ADMIN_MASTER_KEY   мастер-ключ в base64 (иначе data/master.key)
  NAPARNIK_ADMIN_UPSTREAM     базовый адрес API Напарника
`)
}

func runServe(configPath string, log *slog.Logger) error {
	// Если процесс запущен службой Windows, управление берёт на себя SCM.
	if isWindowsService() {
		return runUnderSCM(configPath, log)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveWithContext(ctx, cfg, log)
}

// serveWithContext поднимает сервис и работает до отмены контекста.
func serveWithContext(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	a, err := app.New(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.Start(ctx); err != nil {
		return err
	}
	log.Info("сервис запущен",
		"data_dir", cfg.DataDir,
		"listeners", len(cfg.Listeners),
		"stream_mode", cfg.StreamMode)

	<-ctx.Done()
	log.Info("останавливаемся…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.Shutdown(shutdownCtx)
	return nil
}

func runKey(configPath string, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("укажите действие: add или list")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	masterKey, err := secrets.LoadOrCreateMasterKey(cfg.DataDir)
	if err != nil {
		return err
	}
	cipher, err := secrets.NewCipher(masterKey)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("key add", flag.ExitOnError)
		name := fs.String("name", "", "название ключа")
		key := fs.String("key", "", "ключ Напарника")
		_ = fs.Parse(args[1:])
		if *key == "" {
			return fmt.Errorf("укажите -key")
		}
		enc, err := cipher.Encrypt(*key)
		if err != nil {
			return err
		}
		hint := secrets.KeyHint(*key)
		id, err := st.CreateKey(ctx, orDefault(*name, "Ключ из CLI"), hint, enc)
		if err != nil {
			return err
		}
		fmt.Printf("ключ добавлен, id=%d\n", id)
	case "list":
		keys, err := st.ListKeys(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			fmt.Printf("%d\t%s\t%s\t%s\n", k.ID, k.Name, k.KeyHint, k.Status)
		}
	default:
		return fmt.Errorf("неизвестное действие %q", args[0])
	}
	return nil
}

func runToken(configPath string, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("укажите действие: create или list")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ExitOnError)
		keyID := fs.Int64("key-id", 0, "id ключа Напарника")
		name := fs.String("name", "", "название токена")
		provider := fs.String("provider", "", "протокол: openai | responses | anthropic | gemini (пусто — любой)")
		_ = fs.Parse(args[1:])
		if *keyID == 0 {
			return fmt.Errorf("укажите -key-id (список ключей: naparnik-admin key list)")
		}
		token, err := secrets.NewToken()
		if err != nil {
			return err
		}
		id, err := st.CreateToken(ctx, store.Token{
			KeyID: *keyID, Name: orDefault(*name, "Токен из CLI"), Provider: *provider,
			Prefix: token.Prefix, ServerTools: "auto", Enabled: true,
		}, token.Hash)
		if err != nil {
			return err
		}
		fmt.Printf("токен создан, id=%d\n%s\n", id, token.Plain)
	case "list":
		tokens, err := st.ListTokens(ctx, 0)
		if err != nil {
			return err
		}
		for _, t := range tokens {
			fmt.Printf("%d\tkey=%d\t%s\t%s\t%s\n", t.ID, t.KeyID, t.Name, t.Prefix, t.Provider)
		}
	default:
		return fmt.Errorf("неизвестное действие %q", args[0])
	}
	return nil
}

func runDoctor(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	fmt.Printf("конфигурация: %s\n", configPath)
	fmt.Printf("каталог данных: %s\n", cfg.DataDir)
	fmt.Printf("адрес API Напарника: %s\n", cfg.UpstreamBase)
	fmt.Printf("режим потока: %s\n", cfg.StreamMode)
	for _, l := range cfg.Listeners {
		fmt.Printf("слушатель: %s (%s)\n", l.Addr, l.Kind)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("база: %w", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Check(ctx); err != nil {
		return fmt.Errorf("база не отвечает: %w", err)
	}
	keys, _ := st.ListKeys(ctx)
	tokens, _ := st.ListTokens(ctx, 0)
	fmt.Printf("ключей: %d, токенов: %d\n", len(keys), len(tokens))
	if _, err := secrets.LoadOrCreateMasterKey(cfg.DataDir); err != nil {
		return fmt.Errorf("мастер-ключ: %w", err)
	}
	fmt.Println("мастер-ключ: ок")
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
