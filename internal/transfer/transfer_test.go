package transfer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

// newCipher создаёт шифратор с фиксированным мастер-ключом: тесту не нужен
// файл master.key и предсказуемый результат важнее случайности.
func newCipher(t *testing.T) *secrets.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := secrets.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "transfer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestSealOpenRoundTrip проверяет шифрование файла: верный пароль открывает,
// неверный и короткий — отклоняются, чужой формат не принимается.
func TestSealOpenRoundTrip(t *testing.T) {
	b := &Bundle{
		Format:    Format,
		Version:   Version,
		CreatedAt: time.Now().Truncate(time.Second),
		Keys:      []Key{{Name: "Рабочий", Secret: "5oShdAqK-secret", Hint: "5oShdAqK…cret"}},
		Tokens: []Token{{
			KeyName: "Рабочий", Name: "Cline", Secret: "nr_testtoken",
			SecretAvailable: true, ServerTools: "auto", Enabled: true,
		}},
	}
	data, err := Seal(b, "correct-horse")
	if err != nil {
		t.Fatalf("шифрование: %v", err)
	}
	// В файле не должно быть открытых секретов: это его смысл.
	if strings.Contains(string(data), "5oShdAqK-secret") || strings.Contains(string(data), "nr_testtoken") {
		t.Fatal("секреты попали в файл открытым текстом")
	}

	got, err := Open(data, "correct-horse")
	if err != nil {
		t.Fatalf("расшифровка: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0].Secret != "5oShdAqK-secret" {
		t.Fatalf("ключи после расшифровки: %+v", got.Keys)
	}
	if len(got.Tokens) != 1 || got.Tokens[0].Secret != "nr_testtoken" {
		t.Fatalf("токены после расшифровки: %+v", got.Tokens)
	}

	if _, err := Open(data, "wrong-password"); err == nil {
		t.Fatal("неверный пароль должен отклоняться")
	}
	if _, err := Seal(b, "short"); err == nil {
		t.Fatal("короткий пароль должен отклоняться")
	}
	if _, err := Open([]byte(`{"format":"something-else"}`), "correct-horse"); err == nil {
		t.Fatal("чужой файл должен отклоняться")
	}
	if _, err := Open([]byte("не json"), "correct-horse"); err == nil {
		t.Fatal("повреждённый файл должен отклоняться")
	}

	var metadata envelope
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Cipher = "unsupported"
	incompatible, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(incompatible, "correct-horse"); err == nil {
		t.Fatal("несовместимый формат шифрования должен отклоняться")
	}
}

// TestExportImportRoundTrip проверяет главное обещание переноса: клиенту
// не нужно менять токен. После импорта на «новой машине» тот же токен
// находится по хешу и привязан к тому же ключу.
func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	cipher := newCipher(t)

	// Источник: ключ, токен со значением, слушатели и настройки.
	src := newStore(t)
	keyID, err := src.CreateKey(ctx, "Рабочий", "5oSh…cret", mustEncrypt(t, cipher, "5oShdAqK-secret"))
	if err != nil {
		t.Fatal(err)
	}
	plain := "nr_source_token_value"
	if _, err := src.CreateToken(ctx, store.Token{
		KeyID: keyID, Name: "Cline", Prefix: plain[:11],
		StreamMode: "deltas", ServerTools: "auto", Enabled: true,
	}, secrets.HashToken(plain), mustEncryptString(t, cipher, plain)); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Listeners = []config.Listener{
		{Addr: "127.0.0.1:49031", Kind: config.KindGateway},
		{Addr: "127.0.0.1:49030", Kind: config.KindAdmin},
	}
	cfg.StreamMode = "deltas"
	cfg.ModelID = "naparnik-test"

	bundle, err := Export(ctx, src, cipher, cfg)
	if err != nil {
		t.Fatalf("выгрузка: %v", err)
	}
	if len(bundle.Keys) != 1 || len(bundle.Tokens) != 1 {
		t.Fatalf("в файле не то содержимое: keys=%d tokens=%d", len(bundle.Keys), len(bundle.Tokens))
	}

	// Приёмник: другая база, другой мастер-ключ — как на новой машине.
	dst := newStore(t)
	dstCipher := newCipher(t)
	// В приёмнике уже есть свой ключ: после импорта он должен исчезнуть.
	if _, err := dst.CreateKey(ctx, "Старый", "old", mustEncrypt(t, dstCipher, "old-secret")); err != nil {
		t.Fatal(err)
	}

	res, err := Import(ctx, dst, dstCipher, bundle)
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	if res.Keys != 1 || res.Tokens != 1 || res.Skipped != 0 {
		t.Fatalf("итог импорта: %+v", res)
	}

	// Старого ключа больше нет — замена полная.
	keys, err := dst.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "Рабочий" {
		t.Fatalf("ключи после импорта: %+v", keys)
	}

	// Секрет ключа расшифровывается новым мастер-ключом.
	enc, err := dst.KeySecret(ctx, keys[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if secret, err := dstCipher.Decrypt(enc); err != nil || secret != "5oShdAqK-secret" {
		t.Fatalf("секрет ключа: %q, %v", secret, err)
	}

	// Токен находится по прежнему значению — клиенту менять ничего не нужно.
	tok, err := dst.ResolveToken(ctx, secrets.HashToken(plain))
	if err != nil {
		t.Fatalf("токен не находится по прежнему значению: %v", err)
	}
	if tok.Name != "Cline" || tok.StreamMode != "deltas" || tok.KeyID != keys[0].ID {
		t.Fatalf("токен перенесён неверно: %+v", tok)
	}
	if secretEnc, err := dst.TokenSecret(ctx, tok.ID); err != nil {
		t.Fatal(err)
	} else if secret, err := dstCipher.Decrypt([]byte(secretEnc)); err != nil || secret != plain {
		t.Fatalf("значение токена: %q, %v", secret, err)
	}
}

func TestExportImportPreservesRevocation(t *testing.T) {
	ctx := context.Background()
	src, dst := newStore(t), newStore(t)
	cipher := newCipher(t)
	keyID, err := src.CreateKey(ctx, "fixture", "fixture", mustEncrypt(t, cipher, "fixture-key"))
	if err != nil {
		t.Fatal(err)
	}
	const plain = "fixture-revoked-token"
	hash := secrets.HashToken(plain)
	id, err := src.CreateToken(ctx, store.Token{KeyID: keyID, Name: "fixture", Enabled: true}, hash, mustEncryptString(t, cipher, plain))
	if err != nil {
		t.Fatal(err)
	}
	if err := src.RevokeToken(ctx, id); err != nil {
		t.Fatal(err)
	}
	before, err := src.ResolveToken(ctx, hash)
	if err != nil || before.RevokedAt == nil {
		t.Fatalf("invalid source fixture: %+v, %v", before, err)
	}
	bundle, err := Export(ctx, src, cipher, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	data, err := Seal(bundle, "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err = Open(data, "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, dst, cipher, bundle); err != nil {
		t.Fatal(err)
	}
	after, err := dst.ResolveToken(ctx, hash)
	if err != nil || after.Enabled || after.RevokedAt == nil || !after.RevokedAt.Equal(*before.RevokedAt) {
		t.Fatalf("revocation did not survive transfer: enabled=%t, revoked=%v, error=%v", after.Enabled, after.RevokedAt, err)
	}
}

func TestCancelledReplacementRestoresSavedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const original = "# keep this comment\nmodel_id: original\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ModelID = "replacement"
	st := newStore(t)
	if _, err := st.CreateKey(context.Background(), "original", "original", []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.ReplaceAll(ctx, nil, nil, func() (func() error, error) {
		undo, err := cfg.SaveReversible()
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		return undo, nil
	}); err == nil {
		t.Fatal("cancelled replacement unexpectedly committed")
	}
	saved, err := os.ReadFile(path)
	if err != nil || string(saved) != original {
		t.Fatalf("configuration was not restored: %q, %v", saved, err)
	}
	keys, err := st.ListKeys(context.Background())
	if err != nil || len(keys) != 1 || keys[0].Name != "original" {
		t.Fatalf("failed commit changed the original database: %v, %v", keys, err)
	}
}

// TestImportSkipsTokensWithoutSecret проверяет честность: токен, значение
// которого в исходной базе не сохранено, перенести нельзя — он пропускается
// с подсчётом, а не превращается в нерабочую запись.
func TestImportSkipsTokensWithoutSecret(t *testing.T) {
	ctx := context.Background()
	cipher := newCipher(t)
	dst := newStore(t)

	bundle := &Bundle{
		Format: Format, Version: Version, CreatedAt: time.Now(),
		Keys: []Key{{Name: "К", Secret: "key-secret"}},
		Tokens: []Token{
			{KeyName: "К", Name: "Без значения", SecretAvailable: false},
			{KeyName: "К", Name: "С значением", Secret: "nr_ok", SecretAvailable: true},
		},
	}
	res, err := Import(ctx, dst, cipher, bundle)
	if err != nil {
		t.Fatalf("импорт: %v", err)
	}
	if res.Skipped != 1 || res.Tokens != 1 {
		t.Fatalf("ожидался один пропуск и один токен: %+v", res)
	}
	tokens, err := dst.ListTokens(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].Name != "С значением" {
		t.Fatalf("токены после импорта: %+v", tokens)
	}
}

// TestImportRejectsBrokenBundle проверяет отказ до изменения базы: файл
// с токеном без ключа не должен оставить сервис без ключей вообще.
func TestImportRejectsBrokenBundle(t *testing.T) {
	ctx := context.Background()
	cipher := newCipher(t)
	dst := newStore(t)
	if _, err := dst.CreateKey(ctx, "Живой", "h", mustEncrypt(t, cipher, "s")); err != nil {
		t.Fatal(err)
	}

	broken := &Bundle{
		Format: Format, Version: Version,
		Keys:   []Key{{Name: "К", Secret: "key-secret"}},
		Tokens: []Token{{KeyName: "Нет такого", Name: "T", Secret: "nr_x", SecretAvailable: true}},
	}
	if _, err := Import(ctx, dst, cipher, broken); err == nil {
		t.Fatal("токен с неизвестным ключом должен отклоняться")
	}
	// База осталась прежней: замена не должна была начаться.
	keys, err := dst.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "Живой" {
		t.Fatalf("база изменилась при неудачном импорте: %+v", keys)
	}

	// Ключ без значения — тоже отказ.
	noSecret := &Bundle{Format: Format, Version: Version, Keys: []Key{{Name: "Пустой"}}}
	if _, err := Import(ctx, dst, cipher, noSecret); err == nil {
		t.Fatal("ключ без значения должен отклоняться")
	}
}

func mustEncrypt(t *testing.T, c *secrets.Cipher, plain string) []byte {
	t.Helper()
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func mustEncryptString(t *testing.T, c *secrets.Cipher, plain string) string {
	t.Helper()
	return string(mustEncrypt(t, c, plain))
}
