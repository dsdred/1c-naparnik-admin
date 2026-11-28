package secrets

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestMasterKeyPersistsAndEnvironmentOverrides(t *testing.T) {
	const env = "NAPARNIK_ADMIN_MASTER_KEY"
	t.Setenv(env, "")
	dir := t.TempDir()
	first, err := LoadOrCreateMasterKey(dir)
	if err != nil || len(first) != 32 {
		t.Fatalf("master key creation: size=%d, err=%v", len(first), err)
	}
	second, err := LoadOrCreateMasterKey(dir)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("master key was not loaded from disk: %v", err)
	}
	override := bytes.Repeat([]byte{0xa5}, 32)
	t.Setenv(env, base64.StdEncoding.EncodeToString(override))
	got, err := LoadOrCreateMasterKey(dir)
	if err != nil || !bytes.Equal(got, override) {
		t.Fatalf("environment key was not used: %v", err)
	}
}

func TestCipherAuthenticatesEncryptedSecrets(t *testing.T) {
	c, err := NewCipher(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const plaintext = "test secret"
	first, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("encrypting the same secret twice produced identical ciphertext")
	}
	got, err := c.Decrypt(first)
	if err != nil || got != plaintext {
		t.Fatalf("could not decrypt original ciphertext: %v", err)
	}
	first[len(first)-1] ^= 1
	if _, err := c.Decrypt(first); err == nil {
		t.Fatal("modified ciphertext was accepted")
	}
}

func TestVerifyPassword(t *testing.T) {
	hash, err := HashPassword("fixture password")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("fixture password", hash) {
		t.Fatal("original password was rejected")
	}
	if VerifyPassword("wrong password", hash) {
		t.Fatal("incorrect password was accepted")
	}
}

func TestNewTokenUsesFullValueForHash(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	encoded, ok := strings.CutPrefix(token.Plain, "nr_")
	random, err := base64.RawURLEncoding.DecodeString(encoded)
	if !ok || err != nil || len(random) != 24 {
		t.Fatal("token does not contain 24 bytes of URL-safe randomness")
	}
	if len(token.Prefix) != 11 || !strings.HasPrefix(token.Plain, token.Prefix) {
		t.Fatal("display prefix does not match the token")
	}
	if token.Hash != HashToken(token.Plain) || token.Hash == HashToken(token.Prefix) {
		t.Fatal("token hash is not bound to the full value")
	}
}
