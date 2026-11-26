BINARY := bin/naparnik-admin

# Версия: последний git-тег, иначе dev. Переопределяется: make build VERSION=1.1.0
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "")
DATE    ?= $(shell date +%Y-%m-%d)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(DATE)

# Числовая версия для свойств EXE: 1.1.0 -> 1.1.0.0
NUMVER := $(VERSION)
ifeq ($(words $(subst ., ,$(NUMVER))),3)
NUMVER := $(NUMVER).0
endif

.PHONY: build test vet run clean icon release dist

# Иконка и версия в EXE: собирает rsrc_windows_amd64.syso из брендового .ico.
# Требуется go-winres (go install github.com/tc-hib/go-winres@latest).
icon:
	cd cmd/naparnik-admin && go-winres simply --arch amd64 \
		--icon ../../branding/icons/app.ico \
		--product-name "Naparnik Admin" \
		--file-description "Naparnik Admin — шлюз и админка для 1С:Напарник" \
		--original-filename naparnik-admin.exe --manifest cli \
		--file-version $(NUMVER) --product-version $(NUMVER) --out rsrc

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/naparnik-admin

release: icon build dist

dist:
	cp $(BINARY).exe bin/naparnik-admin-$(VERSION).exe 2>/dev/null || cp $(BINARY) bin/naparnik-admin-$(VERSION)

test:
	go test ./...

vet:
	go vet ./...

run: build
	./$(BINARY) -config config.yaml serve

clean:
	rm -rf bin data data-test
