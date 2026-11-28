package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
	"naparnikadmin/internal/transfer"
)

const importedToken = "fixture-imported-token"

func TestTransferImportLeavesExistingKeysOnFailure(t *testing.T) {
	for _, failure := range []string{"occupied port", "config save"} {
		t.Run(failure, func(t *testing.T) {
			a, path := newConfigTestApp(t)
			admin, cookie := settingsTestLogin(t, a)
			ctx := context.Background()
			encrypted, err := a.Cipher.Encrypt("original-key")
			if err != nil {
				t.Fatal(err)
			}
			id, err := a.Store.CreateKey(ctx, "original", "original", encrypted)
			if err != nil {
				t.Fatal(err)
			}
			originalToken := secrets.HashToken("fixture-original-token")
			if _, err := a.Store.CreateToken(ctx, store.Token{KeyID: id, Name: "original", Enabled: true}, originalToken, ""); err != nil {
				t.Fatal(err)
			}

			bundle := transferFixture(a.Cfg.Snapshot())
			bundle.Settings.ModelID = "imported"
			switch failure {
			case "occupied port":
				occupied, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer occupied.Close()
				bundle.Listeners = []config.Listener{{Kind: config.KindAdmin, Addr: occupied.Addr().String()}}
			case "config save":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			r := postTransfer(t, admin, cookie, bundle)
			if r.Code != http.StatusConflict {
				t.Fatalf("import: status %d, body %s", r.Code, r.Body)
			}
			if got := a.Cfg.Snapshot().ModelName(); got == "imported" {
				t.Fatalf("failed import published the new model: %s", got)
			}
			if _, err := a.Store.ResolveToken(ctx, originalToken); err != nil {
				t.Fatalf("failed import removed the original token: %v", err)
			}
			if _, err := a.Store.ResolveToken(ctx, secrets.HashToken(importedToken)); err == nil {
				t.Fatal("failed import installed the replacement token")
			}
		})
	}
}

func TestTransferImportChangesUpstreamWithoutRestart(t *testing.T) {
	previous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request to previous upstream: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer previous.Close()
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations":
			io.WriteString(w, `{"uuid":"dialog"}`)
		case "/conversations/dialog/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: "+`{"uuid":"answer","role":"assistant","finished":true,"content":{"content":"new upstream"}}`+"\n\n")
		case "/conversations/dialog/context_tokens":
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer replacement.Close()

	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	cfg.UpstreamBase = previous.URL
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	admin, cookie := settingsTestLogin(t, a)
	bundle := transferFixture(cfg)
	bundle.Settings.UpstreamBase = replacement.URL + "/"
	if r := postTransfer(t, admin, cookie, bundle); r.Code != http.StatusOK {
		t.Fatalf("import: %d %s", r.Code, r.Body)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+importedToken)
	w := httptest.NewRecorder()
	a.GatewayHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "new upstream") {
		t.Fatalf("request did not reach the new upstream: %d %s", w.Code, w.Body)
	}
}

func transferFixture(cfg *config.Config) *transfer.Bundle {
	return &transfer.Bundle{
		Format: transfer.Format, Version: transfer.Version, Settings: transfer.SettingsFrom(cfg),
		Keys: []transfer.Key{{Name: "replacement", Secret: "replacement-key"}},
		Tokens: []transfer.Token{{
			KeyName: "replacement", Name: "replacement", Secret: importedToken, SecretAvailable: true, Enabled: true,
		}},
	}
}

func postTransfer(t *testing.T, h http.Handler, cookie *http.Cookie, bundle *transfer.Bundle) *httptest.ResponseRecorder {
	t.Helper()
	const password = "fixture-transfer-password"
	envelope, err := transfer.Seal(bundle, password)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"data": base64.StdEncoding.EncodeToString(envelope), "password": password})
	if err != nil {
		t.Fatal(err)
	}
	return settingsRequest(h, http.MethodPost, "/api/transfer/import", string(body), cookie)
}
