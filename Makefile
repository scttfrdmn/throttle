.PHONY: test race fmt vet check demo demo-serve build cross

test:
	go test ./...

# Reservations are concurrent by design, so the race detector is part of the deal.
race:
	go test -race ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

check: fmt vet race

# VERSION is stamped into a release build. An ordinary "go build" needs none of this: the
# binary derives a development version from the checkout on its own.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := $(if $(VERSION),-X main.version=$(VERSION),)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/throttle ./cmd/throttle

# The platforms a v0.1 release targets. Builds them all into dist/ to prove they compile;
# publishing is a separate, deliberate step.
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

cross:
	@rm -rf dist
	@for p in $(PLATFORMS); do \
		os=$${p%%/*}; arch=$${p##*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -ldflags "$(LDFLAGS)" -o dist/throttle_$${os}_$${arch}/throttle ./cmd/throttle || exit 1; \
	done
	@echo
	@ls -l dist/*/throttle

# Walks the documented first run against a scratch HOME, so the demo never touches the
# user's real configuration or ledger: init, edit, check, apply, status.
DEMO_HOME := /tmp/throttle-demo
DEMO := env HOME=$(DEMO_HOME) XDG_CONFIG_HOME= XDG_DATA_HOME= THROTTLE_CONFIG= THROTTLE_LEDGER= THROTTLE_ACTIVITY= go run ./cmd/throttle

demo:
	@rm -rf $(DEMO_HOME) && mkdir -p $(DEMO_HOME)
	$(DEMO) init
	@echo
	$(DEMO) config check
	@echo
	$(DEMO) config apply
	@echo
	$(DEMO) status -estimate '$$2.50'
	@echo
	@echo 'the dashboard for the same budget: make demo-serve'

demo-serve:
	$(DEMO) serve
