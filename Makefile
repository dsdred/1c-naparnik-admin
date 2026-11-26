BINARY := bin/naparnik-admin

.PHONY: build test vet run clean icon

# Иконка и версия в EXE: собирает rsrc_windows_amd64.syso из брендового .ico.
# Требуется go-winres (go install github.com/tc-hib/go-winres@latest).
icon:
	cd cmd/naparnik-admin && go-winres simply --arch amd64 \
		--icon ../../branding/icons/app.ico \
		--product-name "Naparnik Admin" \
		--file-description "Naparnik Admin — шлюз и админка для 1С:Напарник" \
		--original-filename naparnik-admin.exe --manifest cli --out rsrc

build:
	go build -trimpath -o $(BINARY) ./cmd/naparnik-admin

test:
	go test ./...

vet:
	go vet ./...

run: build
	./$(BINARY) -config config.yaml serve

clean:
	rm -rf bin data data-test
