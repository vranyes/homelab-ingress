ROOT := $(CURDIR)
LIMA_VM ?= hl-ingress-e2e
UNAME_S := $(shell uname -s)

.PHONY: help test test-unit _e2e-vm-boot test-e2e test-e2e-shell test-e2e-bringup test-e2e-down clean-e2e-vm

help: ## Show this help
	@echo "homelab-ingress Makefile targets:"
	@echo "  test                Run unit tests, then e2e suite"
	@echo "  test-unit           Run unit tests (in ingress + controller modules)"
	@echo "  test-e2e            Boot lima VM, build binaries, run e2e tests inside it"
	@echo "  test-e2e-shell      Boot lima VM (idempotent) and drop into an interactive shell"
	@echo "  test-e2e-bringup    Boot lima VM and copy the repo in (no test run)"
	@echo "  test-e2e-down       Stop and delete the lima VM"
	@echo "  clean-e2e-vm        Alias of test-e2e-down"
	@echo ""
	@echo "E2E options:"
	@echo "  E2E_KEEP_VM=1       Leave the VM running after test-e2e (for debugging)"

test: test-unit test-e2e ## Run unit tests, then e2e suite

test-unit: ## Run unit tests in ingress + controller modules
	@echo "==> Running ingress unit tests ($(UNAME_S))"
ifeq ($(UNAME_S),Linux)
	cd ingress && go test ./internal/... -cover -v
else
	cd ingress && go test ./internal/config/... ./internal/macvlan/... -cover -v
endif
	@echo "==> Running controller unit tests ($(UNAME_S))"
	cd controller && go test ./internal/... -cover -v

# Private: idempotently boot the lima VM and copy the repo in.
_e2e-vm-boot:
	@echo "==> Verifying limactl is installed"
	@command -v limactl >/dev/null 2>&1 || { \
		echo "ERROR: limactl not found on PATH."; \
		echo "Install lima:"; \
		echo "  macOS:  brew install lima"; \
		echo "  Linux:  see https://lima-vm.io/docs/installation/"; \
		exit 1; \
	}
	@echo "==> Idempotently starting lima VM '$(LIMA_VM)'"
	-@limactl list 2>/dev/null | grep -q "^$(LIMA_VM) " || \
		limactl start --tty=false --name=$(LIMA_VM) $(ROOT)/e2e-vm/lima.yaml
	@echo "==> Copying repo into VM at /root/homelab-ingress/"
	limactl shell $(LIMA_VM) -- sudo mkdir -p /root/homelab-ingress
	tar czf - --no-xattrs -C $(ROOT) . | limactl shell $(LIMA_VM) -- sudo tar xzf - -C /root/homelab-ingress/

test-e2e: _e2e-vm-boot ## Boot lima VM, build binaries, run e2e suite
	@echo "==> Building binaries and running e2e tests inside the VM"
	limactl shell $(LIMA_VM) -- sudo sh -c '\
		set -e; \
		cd /root/homelab-ingress; \
		export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; \
		export CGO_ENABLED=0; \
		echo "==> Waiting for k3s node to be Ready"; \
		kubectl wait --for=condition=Ready node --all --timeout=60s; \
		echo "==> Building binaries"; \
		go build -o /usr/local/bin/ingress    ./ingress/cmd/ingress; \
		go build -o /usr/local/bin/controller ./controller/cmd/controller; \
		go build -o /usr/local/bin/oidc-mock  ./e2e/cmd/oidc-mock; \
		echo "==> Running e2e tests"; \
		cd e2e && go test -tags=e2e -timeout=25m -v ./... \
	'
	@echo "==> Stopping VM (best effort)"
	@if [ -z "$(E2E_KEEP_VM)" ]; then \
		limactl stop $(LIMA_VM); \
	else \
		echo "==> E2E_KEEP_VM set, leaving VM running (run 'make test-e2e-down' to clean up)"; \
	fi

test-e2e-shell: _e2e-vm-boot ## Idempotently boot the VM, then drop user into an interactive shell
	@echo "==> Entering interactive shell in VM '$(LIMA_VM)'"
	limactl shell $(LIMA_VM) sh

test-e2e-bringup: _e2e-vm-boot ## Boot lima VM and copy repo in (no test run)
	@echo "==> VM '$(LIMA_VM)' is up and repo is at /root/homelab-ingress/"

test-e2e-down: ## Stop and delete the lima VM (best effort)
	-@limactl stop $(LIMA_VM)
	-@limactl delete $(LIMA_VM)

clean-e2e-vm: test-e2e-down ## Alias of test-e2e-down
