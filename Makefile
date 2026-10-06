.PHONY: test build vet fmt

test:
	go test ./... -race -count=1 -shuffle=on

build:
	go build ./...

vet:
	go vet ./...

fmt:
	gofmt -l .
