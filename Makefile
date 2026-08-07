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
	sh -n scripts/install-dante-alpine.sh
	bash -n scripts/install-ubuntu.sh
	bash -n scripts/test-alpine-firewall.sh
	sh -n alpine/wwan-proxy.openrc
	test -x scripts/install-alpine.sh
	test -x scripts/install-dante-alpine.sh
	test -x scripts/install-ubuntu.sh
	test -x scripts/test-alpine-firewall.sh
	test -x alpine/wwan-proxy.openrc
	test -s alpine/wwan-proxy.confd
	test -s alpine/wwan-proxy.logrotate
	grep -F 'SUPPORTED_ALPINE_SERIES="3.21 3.22 3.23"' scripts/install-alpine.sh
	grep -F 'WWAN_PROXY_NPROC_LIMIT="unlimited"' alpine/wwan-proxy.confd
	grep -F -- '-u $${WWAN_PROXY_NPROC_LIMIT:-unlimited}' alpine/wwan-proxy.openrc
	grep -F 'LimitNPROC=infinity' wwan-proxy.service
	grep -F 'TasksMax=infinity' wwan-proxy.service

clean:
	rm -f wwan-proxy
