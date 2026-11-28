#!/usr/bin/env bash
# Собирает релизные бинарники под все поддерживаемые платформы и упаковывает
# их в архивы с контрольными суммами.
#
# Кросс-компиляция идёт с CGO_ENABLED=0: SQLite (modernc.org/sqlite) и всё
# остальное — чистый Go, поэтому отдельные toolchain'ы не нужны. Иконка и
# версия в свойствах EXE подхватываются из cmd/naparnik-admin/rsrc_windows_amd64.syso
# (пересобирается целью `make icon`).
#
# Использование:
#   scripts/build-release.sh                 # версия тега HEAD или dev
#   VERSION=1.5.0 scripts/build-release.sh   # явная версия
#   OUT=/tmp/rel scripts/build-release.sh    # новый каталог выгрузки
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --exact-match HEAD 2>/dev/null || echo dev)}
VERSION=${VERSION#v}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo "")}
DATE=${DATE:-$(date +%Y-%m-%d)}
OUT=${OUT:-dist}

if [[ ! $VERSION =~ ^[a-zA-Z0-9][a-zA-Z0-9._+-]*$ ]]; then
    printf 'Недопустимое имя версии: %s\n' "$VERSION" >&2
    exit 1
fi

LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$DATE"

# GOOS GOARCH имя-в-архиве
TARGETS="
windows amd64 x64
windows arm64 arm64
linux amd64 x64
linux arm64 arm64
darwin amd64 x64
darwin arm64 arm64
"

mkdir -p -- "$(dirname -- "$OUT")"
if ! mkdir -- "$OUT"; then
    printf 'Укажите новый каталог для сборки в OUT.\n' >&2
    exit 1
fi
OUT=$(cd -- "$OUT" && pwd)

# zip_file создаёт .zip: в Linux/macOS есть zip, в Windows Git Bash — нет,
# поэтому там откатываемся на PowerShell.
zip_file() {
    local dir=$1 archive=$2
    if command -v zip >/dev/null 2>&1; then
        (cd "$(dirname "$dir")" && zip -qr "$archive" "$(basename "$dir")")
    else
        NAPARNIK_ZIP_SOURCE=$(cygpath -w "$dir") \
        NAPARNIK_ZIP_TARGET=$(cygpath -w "$archive") \
            powershell -NoProfile -Command '$ErrorActionPreference = "Stop"; Compress-Archive -LiteralPath $env:NAPARNIK_ZIP_SOURCE -DestinationPath $env:NAPARNIK_ZIP_TARGET'
    fi
}

tar_file() {
    local dir=$1 archive=$2 bin=$3
    local parent name
    parent=$(dirname -- "$dir")
    name=$(basename -- "$dir")
    if [[ $OSTYPE == msys* ]]; then
        # NTFS не сохраняет Unix-бит запуска. Задаём режим записи бинарника в tar.
        local raw="${archive%.gz}"
        tar -cf "$raw" -C "$parent" --exclude="$name/$bin" "$name"
        tar -rf "$raw" --mode=0755 -C "$parent" "$name/$bin"
        gzip -- "$raw"
    else
        tar -czf "$archive" -C "$parent" "$name"
    fi
}

echo "naparnik-admin $VERSION ($COMMIT), сборка $DATE"
echo

archives=()
while read -r goos goarch name; do
    [ -z "$goos" ] && continue
    platform=$goos
    [[ $goos == darwin ]] && platform=macos
    stage="$OUT/naparnik-admin-$VERSION-$platform-$name"
    bin="naparnik-admin"
    [ "$goos" = "windows" ] && bin="naparnik-admin.exe"

    GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 \
        go build -trimpath -ldflags "$LDFLAGS" -o "$stage/$bin" ./cmd/naparnik-admin
    chmod +x -- "$stage/$bin"

    # В архиве — бинарник, пример конфигурации, README и unit для сервиса.
    cp config.example.yaml README.md CHANGELOG.md LICENSE "$stage/"
    service=
    case "$goos" in
        linux) service=naparnik-admin.service ;;
        darwin) service=com.naparnik.admin.plist ;;
    esac
    [[ -z $service ]] || cp "deploy/$service" "$stage/"

    case "$goos" in
        windows) archive="$stage.zip"; zip_file "$stage" "$archive" ;;
        *) archive="$stage.tar.gz"; tar_file "$stage" "$archive" "$bin" ;;
    esac

    rm -- "$stage/$bin" "$stage/config.example.yaml" "$stage/README.md" "$stage/CHANGELOG.md" "$stage/LICENSE"
    [[ -z $service ]] || rm -- "$stage/$service"
    rmdir -- "$stage"
    archives+=("$(basename -- "$archive")")
    printf '  %-28s %s\n' "$goos/$goarch" "$(basename -- "$archive")"
done <<< "$TARGETS"

# Контрольные суммы: по ним проверяют скачанный архив перед запуском.
(
    cd -- "$OUT"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -- "${archives[@]}"
    else
        shasum -a 256 -- "${archives[@]}"
    fi
) > "$OUT/SHA256SUMS"

echo
echo "готово: $OUT"
ls -la "$OUT"
