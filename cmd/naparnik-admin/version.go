package main

import "fmt"

// Переменные проставляются при сборке через -ldflags:
//
//	-X main.version=1.1.0 -X main.commit=abc1234 -X main.buildDate=2026-11-26
//
// Без ldflags остаётся "dev" — так собрана локальная отладочная версия.
var (
	version   = "dev"
	commit    = ""
	buildDate = ""
)

// versionLine собирает строку версии для команды version и флага -version.
func versionLine() string {
	s := "naparnik-admin " + version
	if commit != "" {
		s += " (" + commit + ")"
	}
	if buildDate != "" {
		s += ", сборка " + buildDate
	}
	return s
}

func printVersion() {
	fmt.Println(versionLine())
}
