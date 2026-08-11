.PHONY: test race fmt vet check demo

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

# Defines a throttled budget in a scratch ledger and reports its position, so the
# demo never touches the user's real database.
demo:
	@rm -f /tmp/throttle-demo.db
	go run ./cmd/throttle define -db /tmp/throttle-demo.db -id demo -budget 400 -borrow 72h
	@echo
	go run ./cmd/throttle status -db /tmp/throttle-demo.db -id demo -estimate 2.50
