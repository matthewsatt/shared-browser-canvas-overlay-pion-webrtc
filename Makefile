.PHONY: build test e2e run clean

build:
	go build -o bin/collab-browser ./cmd/server

test:
	go vet ./...
	go test ./internal/...

e2e: build
	python3 tests/e2e/run_e2e.py bin/collab-browser

run: build
	./bin/collab-browser

clean:
	rm -rf bin
