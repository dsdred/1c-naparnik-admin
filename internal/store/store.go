// Package store — слой хранения: SQLite (pure Go), схема, репозитории.
package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound возвращается, когда запись отсутствует.
var ErrNotFound = errors.New("не найдено")

// Store — обёртка над базой.
type Store struct {
	db *sql.DB
}

// Key — ключ Напарника (секрет хранится в зашифрованном виде).
type Key struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	KeyHint     string    `json:"key_hint"`
	TariffLabel string    `json:"tariff_label"`
	Status      string    `json:"status"`
	StatusText  string    `json:"status_text"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// Token — токен доступа к шлюзу, привязанный к ключу Напарника.
type Token struct {
	ID          int64      `json:"id"`
	KeyID       int64      `json:"key_id"`
	Name        string     `json:"name"`
	Provider    string     `json:"provider"` // пусто — любой протокол
	Prefix      string     `json:"prefix"`
	StreamMode  string     `json:"stream_mode"`
	ServerTools string     `json:"server_tools"` // auto | deny
	Enabled     bool       `json:"enabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
}

// Session — привязка клиентской сессии к диалогу Напарника.
type Session struct {
	TokenID      int64     `json:"token_id"`
	Protocol     string    `json:"protocol"`
	SessionID    string    `json:"session_id"`
	Conversation string    `json:"conversation"`
	Parent       string    `json:"parent"`
	ToolsJSON    string    `json:"-"`
	Fingerprint  string    `json:"fingerprint"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Sample — замер бюджета ключа.
type Sample struct {
	ID       int64      `json:"id"`
	KeyID    int64      `json:"key_id"`
	TS       time.Time  `json:"ts"`
	Used5h   float64    `json:"used_5h"`
	UsedWeek float64    `json:"used_week"`
	Reset5h  *time.Time `json:"resets_5h"`
	ResetWk  *time.Time `json:"resets_week"`
}

// LogEntry — запись журнала запросов.
type LogEntry struct {
	ID           int64     `json:"id"`
	TS           time.Time `json:"ts"`
	TokenID      int64     `json:"token_id"`
	KeyID        int64     `json:"key_id"`
	Protocol     string    `json:"protocol"`
	Model        string    `json:"model"`
	Status       int       `json:"status"`
	ErrorType    string    `json:"error_type"`
	PromptTokens int       `json:"prompt_tokens"`
	CompTokens   int       `json:"completion_tokens"`
	CachedTokens int       `json:"cached_tokens"`
	LatencyMS    int       `json:"latency_ms"`
	Stream       bool      `json:"stream"`
}

// Open открывает (и при необходимости создаёт) базу.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает базу.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS naparnik_keys (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	key_enc BLOB NOT NULL,
	key_hint TEXT NOT NULL,
	tariff_label TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'unknown',
	status_text TEXT NOT NULL DEFAULT '',
	disabled INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	key_id INTEGER NOT NULL REFERENCES naparnik_keys(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	provider TEXT NOT NULL DEFAULT '',
	prefix TEXT NOT NULL,
	hash TEXT NOT NULL UNIQUE,
	stream_mode TEXT NOT NULL DEFAULT '',
	server_tools TEXT NOT NULL DEFAULT 'auto',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	last_used_at INTEGER,
	revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tokens_key ON tokens(key_id);
CREATE TABLE IF NOT EXISTS sessions (
	token_id INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
	protocol TEXT NOT NULL,
	session_id TEXT NOT NULL,
	conversation TEXT NOT NULL,
	parent TEXT NOT NULL DEFAULT '',
	tools_json TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (token_id, protocol, session_id)
);
CREATE TABLE IF NOT EXISTS budget_samples (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	key_id INTEGER NOT NULL REFERENCES naparnik_keys(id) ON DELETE CASCADE,
	ts INTEGER NOT NULL,
	used_5h REAL NOT NULL,
	used_week REAL NOT NULL,
	resets_5h INTEGER,
	resets_week INTEGER
);
CREATE INDEX IF NOT EXISTS idx_samples_key_ts ON budget_samples(key_id, ts);
CREATE TABLE IF NOT EXISTS request_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	token_id INTEGER NOT NULL DEFAULT 0,
	key_id INTEGER NOT NULL DEFAULT 0,
	protocol TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	error_type TEXT NOT NULL DEFAULT '',
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	stream INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_log_ts ON request_log(ts);
CREATE INDEX IF NOT EXISTS idx_log_key ON request_log(key_id, ts);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`
	_, err := s.db.Exec(schema)
	if err != nil {
		return err
	}
	// Миграция: колонка ide переименована в provider (протокол важнее названия IDE).
	// На новой базе колонки ide нет — ошибка ожидаема и игнорируется.
	_, _ = s.db.Exec(`ALTER TABLE tokens RENAME COLUMN ide TO provider`)
	// Старые значения были названиями IDE, а не протоколами: сбрасываем их в «любой».
	_, _ = s.db.Exec(`UPDATE tokens SET provider='' WHERE provider NOT IN ('', 'openai', 'responses', 'anthropic', 'gemini')`)
	return nil
}

// ---------- ключи ----------

// CreateKey добавляет ключ Напарника.
func (s *Store) CreateKey(ctx context.Context, name, hint string, enc []byte) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO naparnik_keys(name, key_enc, key_hint, created_at) VALUES(?,?,?,?)`,
		name, enc, hint, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateKeySecret заменяет секрет ключа и сбрасывает статус проверки.
func (s *Store) UpdateKeySecret(ctx context.Context, id int64, enc []byte, hint string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE naparnik_keys SET key_enc=?, key_hint=?, status='unknown', status_text='' WHERE id=?`,
		enc, hint, id)
	return err
}

// KeyNameExists сообщает, занято ли название ключа.
// Сравнение без учёта регистра делается в Go: SQLite lower() сворачивает только латиницу.
func (s *Store) KeyNameExists(ctx context.Context, name string, exceptID int64) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM naparnik_keys WHERE id<>?`, exceptID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	want := strings.TrimSpace(name)
	for rows.Next() {
		var id int64
		var have string
		if err := rows.Scan(&id, &have); err != nil {
			return false, err
		}
		if strings.EqualFold(strings.TrimSpace(have), want) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// UpdateKeyHint обновляет отображаемую подсказку ключа.
func (s *Store) UpdateKeyHint(ctx context.Context, id int64, hint string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE naparnik_keys SET key_hint=? WHERE id=?`, hint, id)
	return err
}

// ListKeys возвращает все ключи без секретов.
func (s *Store) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, key_hint, tariff_label, status, status_text, disabled, created_at
		 FROM naparnik_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var created int64
		var disabled int
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHint, &k.TariffLabel, &k.Status, &k.StatusText, &disabled, &created); err != nil {
			return nil, err
		}
		k.Disabled = disabled != 0
		k.CreatedAt = time.Unix(created, 0)
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKey возвращает ключ по id.
func (s *Store) GetKey(ctx context.Context, id int64) (Key, error) {
	var k Key
	var created int64
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, key_hint, tariff_label, status, status_text, disabled, created_at
		 FROM naparnik_keys WHERE id=?`, id).
		Scan(&k.ID, &k.Name, &k.KeyHint, &k.TariffLabel, &k.Status, &k.StatusText, &disabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	if err != nil {
		return k, err
	}
	k.Disabled = disabled != 0
	k.CreatedAt = time.Unix(created, 0)
	return k, nil
}

// KeySecret возвращает шифротекст секрета ключа.
func (s *Store) KeySecret(ctx context.Context, id int64) ([]byte, error) {
	var enc []byte
	err := s.db.QueryRowContext(ctx, `SELECT key_enc FROM naparnik_keys WHERE id=?`, id).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return enc, err
}

// UpdateKeyStatus сохраняет результат проверки ключа.
func (s *Store) UpdateKeyStatus(ctx context.Context, id int64, status, text string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE naparnik_keys SET status=?, status_text=? WHERE id=?`, status, text, id)
	return err
}

// UpdateKeyMeta меняет название, ярлык тарифа и признак отключения.
func (s *Store) UpdateKeyMeta(ctx context.Context, id int64, name, tariff string, disabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE naparnik_keys SET name=?, tariff_label=?, disabled=? WHERE id=?`,
		name, tariff, boolInt(disabled), id)
	return err
}

// DeleteKey удаляет ключ вместе с токенами и сессиями (каскадом).
func (s *Store) DeleteKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM naparnik_keys WHERE id=?`, id)
	return err
}

// ---------- токены ----------

// CreateToken сохраняет токен (хеш) и возвращает id.
func (s *Store) CreateToken(ctx context.Context, t Token, hash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens(key_id, name, provider, prefix, hash, stream_mode, server_tools, enabled, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		t.KeyID, t.Name, t.Provider, t.Prefix, hash, t.StreamMode, t.ServerTools, boolInt(t.Enabled), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListTokens возвращает токены, при необходимости — только для одного ключа.
func (s *Store) ListTokens(ctx context.Context, keyID int64) ([]Token, error) {
	q := `SELECT id, key_id, name, provider, prefix, stream_mode, server_tools, enabled, created_at, last_used_at, revoked_at FROM tokens`
	args := []any{}
	if keyID > 0 {
		q += ` WHERE key_id=?`
		args = append(args, keyID)
	}
	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var enabled, created int
		var lastUsed, revoked sql.NullInt64
		if err := rows.Scan(&t.ID, &t.KeyID, &t.Name, &t.Provider, &t.Prefix, &t.StreamMode, &t.ServerTools,
			&enabled, &created, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		t.Enabled = enabled != 0
		t.CreatedAt = time.Unix(int64(created), 0)
		if lastUsed.Valid {
			v := time.Unix(lastUsed.Int64, 0)
			t.LastUsedAt = &v
		}
		if revoked.Valid {
			v := time.Unix(revoked.Int64, 0)
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResolveToken ищет активный токен по хешу.
func (s *Store) ResolveToken(ctx context.Context, hash string) (Token, error) {
	var t Token
	var enabled, created int
	var lastUsed, revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, key_id, name, provider, prefix, stream_mode, server_tools, enabled, created_at, last_used_at, revoked_at
		 FROM tokens WHERE hash=?`, hash).
		Scan(&t.ID, &t.KeyID, &t.Name, &t.Provider, &t.Prefix, &t.StreamMode, &t.ServerTools, &enabled, &created, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.Enabled = enabled != 0
	t.CreatedAt = time.Unix(int64(created), 0)
	if lastUsed.Valid {
		v := time.Unix(lastUsed.Int64, 0)
		t.LastUsedAt = &v
	}
	if revoked.Valid {
		v := time.Unix(revoked.Int64, 0)
		t.RevokedAt = &v
	}
	return t, nil
}

// TouchToken отмечает время последнего использования.
func (s *Store) TouchToken(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at=? WHERE id=?`, time.Now().Unix(), id)
}

// UpdateToken меняет название, провайдера, режим потока, политику серверных инструментов и активность.
func (s *Store) UpdateToken(ctx context.Context, t Token) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET name=?, provider=?, stream_mode=?, server_tools=?, enabled=? WHERE id=?`,
		t.Name, t.Provider, t.StreamMode, t.ServerTools, boolInt(t.Enabled), t.ID)
	return err
}

// RevokeToken помечает токен отозванным.
func (s *Store) RevokeToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET enabled=0, revoked_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

// ---------- сессии ----------

// GetSession читает привязку сессии.
func (s *Store) GetSession(ctx context.Context, tokenID int64, protocol, sessionID string) (Session, error) {
	var ss Session
	var created, updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT token_id, protocol, session_id, conversation, parent, tools_json, fingerprint, created_at, updated_at
		 FROM sessions WHERE token_id=? AND protocol=? AND session_id=?`, tokenID, protocol, sessionID).
		Scan(&ss.TokenID, &ss.Protocol, &ss.SessionID, &ss.Conversation, &ss.Parent, &ss.ToolsJSON, &ss.Fingerprint, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ss, ErrNotFound
	}
	if err != nil {
		return ss, err
	}
	ss.CreatedAt = time.Unix(created, 0)
	ss.UpdatedAt = time.Unix(updated, 0)
	return ss, nil
}

// SaveSession создаёт или обновляет привязку.
func (s *Store) SaveSession(ctx context.Context, ss Session) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(token_id, protocol, session_id, conversation, parent, tools_json, fingerprint, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(token_id, protocol, session_id) DO UPDATE SET
		   conversation=excluded.conversation, parent=excluded.parent,
		   tools_json=excluded.tools_json, fingerprint=excluded.fingerprint, updated_at=excluded.updated_at`,
		ss.TokenID, ss.Protocol, ss.SessionID, ss.Conversation, ss.Parent, ss.ToolsJSON, ss.Fingerprint, now, now)
	return err
}

// DeleteSession удаляет привязку (сброс диалога).
func (s *Store) DeleteSession(ctx context.Context, tokenID int64, protocol, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_id=? AND protocol=? AND session_id=?`, tokenID, protocol, sessionID)
	return err
}

// ListSessions возвращает привязки для отображения в админке.
func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT token_id, protocol, session_id, conversation, parent, '', fingerprint, created_at, updated_at
		 FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var ss Session
		var created, updated int64
		if err := rows.Scan(&ss.TokenID, &ss.Protocol, &ss.SessionID, &ss.Conversation, &ss.Parent, &ss.ToolsJSON,
			&ss.Fingerprint, &created, &updated); err != nil {
			return nil, err
		}
		ss.CreatedAt = time.Unix(created, 0)
		ss.UpdatedAt = time.Unix(updated, 0)
		out = append(out, ss)
	}
	return out, rows.Err()
}

// PurgeSessions удаляет привязки старше указанного момента.
func (s *Store) PurgeSessions(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE updated_at < ?`, olderThan.Unix())
	return err
}

// ---------- бюджет ----------

// AddSample сохраняет замер бюджета.
func (s *Store) AddSample(ctx context.Context, sm Sample) error {
	var r5, rw any
	if sm.Reset5h != nil {
		r5 = sm.Reset5h.Unix()
	}
	if sm.ResetWk != nil {
		rw = sm.ResetWk.Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO budget_samples(key_id, ts, used_5h, used_week, resets_5h, resets_week) VALUES(?,?,?,?,?,?)`,
		sm.KeyID, sm.TS.Unix(), sm.Used5h, sm.UsedWeek, r5, rw)
	return err
}

// LatestSample возвращает последний замер ключа.
func (s *Store) LatestSample(ctx context.Context, keyID int64) (Sample, error) {
	var sm Sample
	var ts int64
	var r5, rw sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, key_id, ts, used_5h, used_week, resets_5h, resets_week FROM budget_samples
		 WHERE key_id=? ORDER BY ts DESC LIMIT 1`, keyID).
		Scan(&sm.ID, &sm.KeyID, &ts, &sm.Used5h, &sm.UsedWeek, &r5, &rw)
	if errors.Is(err, sql.ErrNoRows) {
		return sm, ErrNotFound
	}
	if err != nil {
		return sm, err
	}
	sm.TS = time.Unix(ts, 0)
	if r5.Valid {
		v := time.Unix(r5.Int64, 0)
		sm.Reset5h = &v
	}
	if rw.Valid {
		v := time.Unix(rw.Int64, 0)
		sm.ResetWk = &v
	}
	return sm, nil
}

// Samples возвращает замеры ключа начиная с момента since.
func (s *Store) Samples(ctx context.Context, keyID int64, since time.Time) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key_id, ts, used_5h, used_week, resets_5h, resets_week FROM budget_samples
		 WHERE key_id=? AND ts>=? ORDER BY ts`, keyID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var sm Sample
		var ts int64
		var r5, rw sql.NullInt64
		if err := rows.Scan(&sm.ID, &sm.KeyID, &ts, &sm.Used5h, &sm.UsedWeek, &r5, &rw); err != nil {
			return nil, err
		}
		sm.TS = time.Unix(ts, 0)
		if r5.Valid {
			v := time.Unix(r5.Int64, 0)
			sm.Reset5h = &v
		}
		if rw.Valid {
			v := time.Unix(rw.Int64, 0)
			sm.ResetWk = &v
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// ---------- журнал ----------

// LogRequest пишет запись журнала.
func (s *Store) LogRequest(ctx context.Context, e LogEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO request_log(ts, token_id, key_id, protocol, model, status, error_type,
		 prompt_tokens, completion_tokens, cached_tokens, latency_ms, stream)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TS.Unix(), e.TokenID, e.KeyID, e.Protocol, e.Model, e.Status, e.ErrorType,
		e.PromptTokens, e.CompTokens, e.CachedTokens, e.LatencyMS, boolInt(e.Stream))
	return err
}

// LogFilter — фильтр журнала.
type LogFilter struct {
	KeyID    int64
	TokenID  int64
	Protocol string
	Status   int
	Since    time.Time
	Limit    int
	Offset   int
}

// QueryLog возвращает записи журнала и общее количество.
func (s *Store) QueryLog(ctx context.Context, f LogFilter) ([]LogEntry, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	if f.KeyID > 0 {
		where += " AND key_id=?"
		args = append(args, f.KeyID)
	}
	if f.TokenID > 0 {
		where += " AND token_id=?"
		args = append(args, f.TokenID)
	}
	if f.Protocol != "" {
		where += " AND protocol=?"
		args = append(args, f.Protocol)
	}
	if f.Status > 0 {
		where += " AND status=?"
		args = append(args, f.Status)
	}
	if !f.Since.IsZero() {
		where += " AND ts>=?"
		args = append(args, f.Since.Unix())
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_log `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q := `SELECT id, ts, token_id, key_id, protocol, model, status, error_type,
	         prompt_tokens, completion_tokens, cached_tokens, latency_ms, stream
	      FROM request_log ` + where + ` ORDER BY ts DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts int64
		var stream int
		if err := rows.Scan(&e.ID, &ts, &e.TokenID, &e.KeyID, &e.Protocol, &e.Model, &e.Status, &e.ErrorType,
			&e.PromptTokens, &e.CompTokens, &e.CachedTokens, &e.LatencyMS, &stream); err != nil {
			return nil, 0, err
		}
		e.TS = time.Unix(ts, 0)
		e.Stream = stream != 0
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Usage — агрегированный расход по ключу или токену.
type Usage struct {
	Requests   int     `json:"requests"`
	Errors     int     `json:"errors"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	Cached     int     `json:"cached_tokens"`
	AvgLatency int     `json:"avg_latency_ms"`
	SuccessPct float64 `json:"success_pct"`
}

// UsageForKey считает агрегаты по ключу с момента since.
func (s *Store) UsageForKey(ctx context.Context, keyID int64, since time.Time) (Usage, error) {
	var u Usage
	var avgLatency sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		        COALESCE(SUM(cached_tokens),0), COALESCE(AVG(latency_ms),0)
		 FROM request_log WHERE key_id=? AND ts>=?`, keyID, since.Unix()).
		Scan(&u.Requests, &u.Errors, &u.Prompt, &u.Completion, &u.Cached, &avgLatency)
	if err != nil {
		return u, err
	}
	// AVG возвращается как REAL — приводим к int отдельно, иначе Scan падает.
	if avgLatency.Valid {
		u.AvgLatency = int(avgLatency.Float64)
	}
	if u.Requests > 0 {
		u.SuccessPct = float64(u.Requests-u.Errors) / float64(u.Requests) * 100
	}
	return u, nil
}

// TokenUsage — расход по конкретному токену.
type TokenUsage struct {
	TokenID    int64   `json:"token_id"`
	TokenName  string  `json:"token_name"`
	Provider   string  `json:"provider"`
	Requests   int     `json:"requests"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	LastUsedAt *string `json:"last_used_at"`
}

// UsageByToken разбивает расход ключа по токенам.
func (s *Store) UsageByToken(ctx context.Context, keyID int64, since time.Time) ([]TokenUsage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.name, t.provider,
		        COUNT(l.id), COALESCE(SUM(l.prompt_tokens),0), COALESCE(SUM(l.completion_tokens),0),
		        MAX(l.ts)
		 FROM tokens t LEFT JOIN request_log l
		   ON l.token_id=t.id AND l.ts>=?
		 WHERE t.key_id=?
		 GROUP BY t.id ORDER BY COUNT(l.id) DESC`, since.Unix(), keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenUsage
	for rows.Next() {
		var u TokenUsage
		var last sql.NullInt64
		if err := rows.Scan(&u.TokenID, &u.TokenName, &u.Provider, &u.Requests, &u.Prompt, &u.Completion, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			v := time.Unix(last.Int64, 0).UTC().Format(time.RFC3339)
			u.LastUsedAt = &v
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SeriesPoint — точка временного ряда для графиков.
type SeriesPoint struct {
	TS       time.Time `json:"ts"`
	Requests int       `json:"requests"`
	Tokens   int       `json:"tokens"`
	UsedWeek float64   `json:"used_week"`
	Used5h   float64   `json:"used_5h"`
}

// SeriesByHour отдаёт почасовой ряд запросов и последний замер бюджета в каждом часе.
func (s *Store) SeriesByHour(ctx context.Context, keyID int64, since time.Time) ([]SeriesPoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT (ts/3600)*3600 AS bucket,
		        COUNT(*), COALESCE(SUM(prompt_tokens+completion_tokens),0)
		 FROM request_log WHERE key_id=? AND ts>=? GROUP BY bucket ORDER BY bucket`, keyID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := map[int64]*SeriesPoint{}
	var order []int64
	for rows.Next() {
		var bucket int64
		var reqs, tokens int
		if err := rows.Scan(&bucket, &reqs, &tokens); err != nil {
			return nil, err
		}
		points[bucket] = &SeriesPoint{TS: time.Unix(bucket, 0).UTC(), Requests: reqs, Tokens: tokens}
		order = append(order, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	srows, err := s.db.QueryContext(ctx,
		`SELECT (ts/3600)*3600 AS bucket, used_5h, used_week FROM budget_samples
		 WHERE key_id=? AND ts>=? ORDER BY ts`, keyID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var bucket int64
		var u5, uw float64
		if err := srows.Scan(&bucket, &u5, &uw); err != nil {
			return nil, err
		}
		p, ok := points[bucket]
		if !ok {
			p = &SeriesPoint{TS: time.Unix(bucket, 0).UTC()}
			points[bucket] = p
			order = append(order, bucket)
		}
		p.Used5h = u5
		p.UsedWeek = uw
	}
	out := make([]SeriesPoint, 0, len(order))
	for _, b := range order {
		out = append(out, *points[b])
	}
	return out, srows.Err()
}

// ---------- настройки ----------

// GetSetting читает значение настройки.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting записывает настройку.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Check делает простую проверку работоспособности базы.
func (s *Store) Check(ctx context.Context) error {
	var n int
	return s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
}
