BINARY := bin/naparnik-admin

.PHONY: build test vet run clean

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
