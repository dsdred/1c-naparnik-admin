package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets/*
var assets embed.FS

// FS возвращает файловую систему со статикой админки.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

// Handler отдаёт SPA: любые неизвестные пути возвращают index.html.
func Handler() http.Handler {
	fsys := FS()
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Интерфейс вшит в бинарник и меняется вместе с ним: без этого браузер
		// держит старые app.js и style.css и показывает устаревшую вёрстку.
		w.Header().Set("Cache-Control", "no-cache")
		if r.URL.Path != "/" {
			if f, err := fsys.Open(trimSlash(r.URL.Path)); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		data, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.Error(w, "не удалось загрузить интерфейс", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
}

func trimSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}
