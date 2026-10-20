BINARY := noderouter
GOFLAGS ?=

.PHONY: all build test race vet fmt fmt-check clean install run convert check routes

all: vet test build

build:
	go build $(GOFLAGS) -o $(BINARY) .

install:
	go install $(GOFLAGS) .

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w *.go

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

run: build
	./$(BINARY)

convert: build
	./$(BINARY) convert

check: build
	./$(BINARY) check

routes: build
	./$(BINARY) routes

clean:
	rm -f $(BINARY)
