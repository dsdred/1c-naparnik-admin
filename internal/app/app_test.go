package app

import (
	"testing"

	"naparnikadmin/internal/config"
)

// TestListenerKeyIncludesTLS проверяет, что смена сертификата меняет ключ
// слушателя. Иначе «Применить» в админке оставил бы старый сертификат
// работать до перезапуска процесса.
func TestListenerKeyIncludesTLS(t *testing.T) {
	plain := config.Listener{Addr: "127.0.0.1:47831", Kind: config.KindGateway}
	withCert := plain
	withCert.CertFile = "a.crt"
	withCert.KeyFile = "a.key"
	otherCert := plain
	otherCert.CertFile = "b.crt"
	otherCert.KeyFile = "b.key"

	if listenerKey(plain) == listenerKey(withCert) {
		t.Fatal("включение HTTPS должно менять ключ слушателя")
	}
	if listenerKey(withCert) == listenerKey(otherCert) {
		t.Fatal("другой сертификат должен менять ключ слушателя")
	}
	if listenerKey(plain) != listenerKey(config.Listener{Addr: plain.Addr, Kind: plain.Kind}) {
		t.Fatal("одинаковые слушатели должны иметь одинаковый ключ")
	}
}
