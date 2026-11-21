VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build run test dist clean demo

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o airadminer .

run: build
	./airadminer

test:
	go vet ./...
	go test ./...
	node --check web/app.js

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
