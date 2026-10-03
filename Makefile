all: check

.PHONY: all test race benchmark fmt fmt-check vet check

test:
	go test ./...

race:
	go test -race ./...

benchmark:
	go test -run=^$$ -bench=. -benchmem ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

check: fmt-check vet test race
