.PHONY: all test check check-scripts clean

all: wwan-proxy

wwan-proxy:
	go build -trimpath -ldflags "-s -w" -o $@ ./cmd/wwan-proxy

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...
	$(MAKE) check-scripts

check-scripts:
	sh -n scripts/install-alpine.sh
	bash -n scripts/test-alpine-firewall.sh
	sh -n alpine/wwan-proxy.openrc
	test -x scripts/install-alpine.sh
	test -x scripts/test-alpine-firewall.sh
	test -x alpine/wwan-proxy.openrc
	test -s alpine/wwan-proxy.confd
	test -s alpine/wwan-proxy.logrotate
	grep -F 'WWAN_PROXY_NPROC_LIMIT="unlimited"' alpine/wwan-proxy.confd
	grep -F -- '-u $${WWAN_PROXY_NPROC_LIMIT:-unlimited}' alpine/wwan-proxy.openrc

clean:
	rm -f wwan-proxy
