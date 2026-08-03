.PHONY: all test check clean

all: wwan-proxy

wwan-proxy:
	go build -trimpath -ldflags "-s -w" -o $@ ./cmd/wwan-proxy

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...

clean:
	rm -f wwan-proxy
