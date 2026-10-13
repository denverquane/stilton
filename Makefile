LIBRARY ?= $(HOME)/3d-models

.PHONY: build web dev dev-web test clean

## build: build the frontend and a single self-contained binary at ./stilton
build: web
	go build -o stilton ./cmd/stilton

web: web/node_modules
	cd web && npm run build

web/node_modules: web/package.json web/package-lock.json
	cd web && npm install
	@touch $@

## dev: run the Go server against $(LIBRARY) (pair with `make dev-web`)
dev:
	go run ./cmd/stilton -library "$(LIBRARY)"

## dev-web: Vite dev server with hot reload, proxying /api and /thumbs to `make dev`
dev-web: web/node_modules
	cd web && npm run dev

test:
	go test ./...
	cd web && npx tsc --noEmit

clean:
	rm -f stilton
	find web/dist -mindepth 1 ! -name .gitkeep -delete
