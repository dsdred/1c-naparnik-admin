package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
	"naparnikadmin/internal/transfer"
)

// passwordFrom решает, откуда взять пароль файла переноса: флаг -password,
// -password-stdin (первая строка стандартного ввода) или интерактивный запрос.
// Пароль не печатаем и не логируем.
func passwordFrom(fs *flag.FlagSet, flagValue string, useStdin bool) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if useStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return "", fmt.Errorf("не удалось прочитать пароль со стандартного ввода: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "Пароль файла переноса: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("не удалось прочитать пароль: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// runExport выгружает ключи, токены, слушатели и настройки в файл под паролем.
func runExport(configPath string, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	out := fs.String("out", "naparnik-transfer.json", "файл выгрузки")
	password := fs.String("password", "", "пароль шифрования файла (иначе будет запрошен)")
	passwordStdin := fs.Bool("password-stdin", false, "читать пароль из первой строки stdin")
	_ = fs.Parse(args)

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
	pass, err := passwordFrom(fs, *password, *passwordStdin)
	if err != nil {
		return err
	}

	bundle, err := transfer.Export(context.Background(), st, cipher, cfg)
	if err != nil {
		return err
	}
	if err := transfer.WriteFile(*out, bundle, pass); err != nil {
		return err
	}
	log.Info("файл переноса выгружен", "file", *out, "keys", len(bundle.Keys), "tokens", len(bundle.Tokens))
	fmt.Printf("выгружено в %s: ключей %d, токенов %d, слушателей %d\n",
		*out, len(bundle.Keys), len(bundle.Tokens), len(bundle.Listeners))
	return nil
}

// runImport переносит настройки из файла. Существующие ключи и токены
// заменяются целиком: это переезд, а не добавление.
func runImport(configPath string, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	in := fs.String("in", "", "файл переноса")
	password := fs.String("password", "", "пароль файла (иначе будет запрошен)")
	passwordStdin := fs.Bool("password-stdin", false, "читать пароль из первой строки stdin")
	_ = fs.Parse(args)
	if *in == "" {
		return fmt.Errorf("укажите -in FILE (файл, выгруженный командой export)")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	pass, err := passwordFrom(fs, *password, *passwordStdin)
	if err != nil {
		return err
	}
	bundle, err := transfer.ReadFile(*in, pass)
	if err != nil {
		return err
	}
	// Проверяем до замены ключей: битый файл не должен оставить сервис без ключей.
	if len(bundle.Listeners) > 0 {
		if err := config.ValidateListeners(bundle.Listeners); err != nil {
			return fmt.Errorf("слушатели в файле: %w", err)
		}
	}
	next := *cfg
	if err := bundle.Settings.Apply(&next); err != nil {
		return fmt.Errorf("настройки в файле: %w", err)
	}
	if len(bundle.Listeners) > 0 {
		next.Listeners = bundle.Listeners
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
	res, err := transfer.ImportWithConfig(context.Background(), st, cipher, bundle, &next)
	if err != nil {
		return err
	}

	log.Info("настройки перенесены", "keys", res.Keys, "tokens", res.Tokens, "skipped", res.Skipped)
	fmt.Printf("перенесено: ключей %d, токенов %d, слушателей %d\n", res.Keys, res.Tokens, len(bundle.Listeners))
	if res.Skipped > 0 {
		fmt.Printf("пропущено токенов без сохранённого значения: %d — выпустите их заново в админке\n", res.Skipped)
	}
	fmt.Println("перезапустите сервис, чтобы применились слушатели и таймауты")
	return nil
}
