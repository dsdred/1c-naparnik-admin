package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TransferKey — ключ Напарника для импорта. Секрет уже зашифрован мастер-ключом
// целевого экземпляра: пакет переноса не знает, каким ключом шифровать здесь.
type TransferKey struct {
	Name        string
	Hint        string
	Enc         []byte
	TariffLabel string
	Disabled    bool
}

// TransferToken — токен для импорта. Hash и шифртекст значения готовы заранее,
// поэтому клиентам не нужно менять токены после переезда.
type TransferToken struct {
	KeyName     string
	Name        string
	Provider    string
	Prefix      string
	Hash        string
	Enc         string
	StreamMode  string
	ServerTools string
	Enabled     bool
	RevokedAt   *time.Time
}

// ReplaceAll заменяет ключи и токены целиком: старые удаляются вместе с
// привязками сессий, контрольными точками и замерами бюджета (каскад по ключу).
// Журнал запросов остаётся — это история, а не настройки, но его записи будут
// ссылаться на прежние id ключей и токенов.
// stage сохраняет связанные настройки после проверки всех записей и даёт
// откат, если SQLite не сможет зафиксировать транзакцию.
func (s *Store) ReplaceAll(ctx context.Context, keys []TransferKey, tokens []TransferToken, stage func() (func() error, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM naparnik_keys`); err != nil {
		return fmt.Errorf("очистка ключей: %w", err)
	}

	ids := make(map[string]int64, len(keys))
	now := time.Now().Unix()
	for _, k := range keys {
		if k.Name == "" {
			return errors.New("ключ без названия")
		}
		if _, dup := ids[k.Name]; dup {
			return fmt.Errorf("ключ %q встречается дважды", k.Name)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO naparnik_keys(name, key_enc, key_hint, tariff_label, status, status_text, disabled, created_at)
			 VALUES(?,?,?,?, 'unknown', '', ?, ?)`,
			k.Name, k.Enc, k.Hint, k.TariffLabel, boolInt(k.Disabled), now)
		if err != nil {
			return fmt.Errorf("ключ %q: %w", k.Name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		ids[k.Name] = id
	}

	for _, t := range tokens {
		keyID, ok := ids[t.KeyName]
		if !ok {
			return fmt.Errorf("токен %q ссылается на неизвестный ключ %q", t.Name, t.KeyName)
		}
		var revoked any
		if t.RevokedAt != nil {
			revoked = t.RevokedAt.Unix()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tokens(key_id, name, provider, prefix, hash, secret_enc, stream_mode, server_tools, enabled, created_at, revoked_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			keyID, t.Name, t.Provider, t.Prefix, t.Hash, t.Enc, t.StreamMode, t.ServerTools, boolInt(t.Enabled && t.RevokedAt == nil), now, revoked); err != nil {
			return fmt.Errorf("токен %q: %w", t.Name, err)
		}
	}
	if stage == nil {
		return tx.Commit()
	}
	undo, err := stage()
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if restoreErr := undo(); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("восстановление конфигурации: %w", restoreErr))
		}
		return err
	}
	return nil
}
