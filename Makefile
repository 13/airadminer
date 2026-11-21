VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build run test test-integration e2e fuzz lint dist clean demo

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o airadminer .

run: build
	./airadminer

test:
	go vet ./...
	go test -race ./...
	node --check web/app.js

# Needs `make demo` (or any PostgreSQL superuser DSN in AIRADMINER_TEST_DSN)
test-integration:
	AIRADMINER_TEST_DSN=$${AIRADMINER_TEST_DSN:-postgres://postgres:secret@127.0.0.1:55432/postgres} go test -race -count=1 ./...

# Browser tests of the grid in headless Chrome (needs `make demo` and google-chrome)
e2e: build
	./e2e/run.sh

FUZZTIME ?= 30s
fuzz:
	@for f in $$(grep -ho '^func Fuzz[A-Za-z]*' *_test.go | cut -c6-); do \
		echo "  $$f"; go test -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) . || exit 1; \
	done

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Static binaries for every platform in ./dist
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/airadminer-$$os-$$arch$$ext . || exit 1; \
	done

# Throwaway PostgreSQL with sample data on port 55432 (password: secret)
demo:
	docker run -d --name airadminer-pg -e POSTGRES_PASSWORD=secret -p 55432:5432 postgres:17-alpine
	@until docker exec airadminer-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	docker exec -i airadminer-pg psql -q -U postgres < testdata/seed.sql

clean:
	rm -rf airadminer dist
