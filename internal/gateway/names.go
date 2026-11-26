package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"naparnikadmin/internal/naparnik"
)

var wireNameRe = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,48}$`)

// WireName превращает имя клиентского инструмента в имя, которое безопасно
// отправлять в Напарника. Напарник резервирует часть имён, поэтому все
// клиентские инструменты получают префикс.
func WireName(name string) string {
	if wireNameRe.MatchString(name) {
		return "client_" + name
	}
	sum := sha256.Sum256([]byte(name))
	digest := hex.EncodeToString(sum[:])[:12]
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	short := b.String()
	if len(short) > 36 {
		short = short[:36]
	}
	return "cx_" + short + "_" + digest
}

// wireToolMap готовит инструменты для отправки и карту обратного переименования.
func wireToolMap(tools []Tool) ([]naparnik.Tool, map[string]string) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]naparnik.Tool, 0, len(tools))
	back := make(map[string]string, len(tools))
	for _, t := range tools {
		wire := WireName(t.Name)
		back[wire] = t.Name
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, naparnik.Tool{Name: wire, Description: t.Description, Parameters: schema})
	}
	return out, back
}
