ROOT := $(CURDIR)
LIMA_VM ?= hl-ingress-e2e
BUILDER_VM := hl-ingress-e2e-builder
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
export LIMA_HOME ?= $(ROOT)/.lima
# Lima-specific build variables
BIN_DIR      := bin
LIMA_GOOS    := linux   # Target OS for Lima VM
# Detect host architecture → native VM arch + Go build target
ifeq ($(UNAME_M),arm64)
  LIMA_ARCH   := aarch64
  LIMA_GOARCH := arm64
else
  LIMA_ARCH   := x86_64
  LIMA_GOARCH := amd64
endif

# CoW-aware disk copy: clonefile on macOS, reflink on Linux
ifeq ($(UNAME_S),Linux)
  DISK_CP = cp --reflink=auto
else
  DISK_CP = cp -c
endif

E2E_DIR := $(ROOT)/.e2e-vm
BASE_DISK := $(E2E_DIR)/disk.base
E2E_MARKER := $(E2E_DIR)/.build-checksum

ifeq ($(UNAME_S),Linux)
  HASH_CMD = sha256sum
else
  HASH_CMD = shasum -a 256
endif

.PHONY: help test test-unit \
        start-e2e-vm remove-e2e-vm ensure-e2e-vm copy-e2e-vm \
        build-e2e-binaries clean-e2e-binaries \
        test-e2e test-e2e-down test-e2e-bringup test-e2e-shell

# E2E binaries (built on host for Lima VM)
$(BIN_DIR)/ingress: $(shell find ingress/cmd/ingress -name '*.go') \
                    $(shell find ingress/internal -name '*.go')
	mkdir -p $(BIN_DIR)
	GOOS=$(LIMA_GOOS) GOARCH=$(LIMA_GOARCH) CGO_ENABLED=0 \
		go build -o $@ ./ingress/cmd/ingress

$(BIN_DIR)/controller: $(shell find controller/cmd/controller -name '*.go') \
                       $(shell find controller/internal -name '*.go')
	mkdir -p $(BIN_DIR)
	GOOS=$(LIMA_GOOS) GOARCH=$(LIMA_GOARCH) CGO_ENABLED=0 \
		go build -o $@ ./controller/cmd/controller

$(BIN_DIR)/oidc-mock: $(shell find e2e/cmd/oidc-mock -name '*.go')
	mkdir -p $(BIN_DIR)
	GOOS=$(LIMA_GOOS) GOARCH=$(LIMA_GOARCH) CGO_ENABLED=0 \
		go build -o $@ ./e2e/cmd/oidc-mock

.PHONY: build-e2e-binaries
build-e2e-binaries: $(BIN_DIR)/ingress $(BIN_DIR)/controller $(BIN_DIR)/oidc-mock

.PHONY: clean-e2e-binaries
clean-e2e-binaries:
	rm -rf $(BIN_DIR)

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

# ------------------------------------------------------------------
# E2E VM lifecycle
# ------------------------------------------------------------------

$(E2E_DIR):
	mkdir -p $(E2E_DIR)

# Lima start flags: arch-aware image + VZ on macOS arm64
LIMA_IMAGE := https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/nocloud_alpine-3.22.4-$(LIMA_ARCH)-uefi-cloudinit-r0.qcow2
ifeq ($(UNAME_M),arm64)
  LIMA_START_FLAGS := --vm-type=vz --set '.arch="$(LIMA_ARCH)"' --rosetta \
                      --set '.images=[{"location":"$(LIMA_IMAGE)","arch":"$(LIMA_ARCH)"}]'
else
  LIMA_START_FLAGS := --set '.images=[{"location":"$(LIMA_IMAGE)","arch":"$(LIMA_ARCH)"}]'
endif

.PHONY: start-e2e-vm
start-e2e-vm:
	@echo "==> Booting clean VM from base image ($(LIMA_ARCH))"
	limactl start --tty=false --name=$(LIMA_VM) $(LIMA_START_FLAGS) $(ROOT)/e2e-vm/lima.yaml

remove-e2e-vm: ## Stop and delete the running VM instance
	-@limactl stop $(LIMA_VM) 2>/dev/null || true
	-@limactl delete $(LIMA_VM) 2>/dev/null || true

# ------------------------------------------------------------------
# E2E test targets
# ------------------------------------------------------------------

.PHONY: ensure-e2e-vm copy-e2e-vm copy-ingress-vm test-unit-vm test-e2e-bringup test-e2e-shell

ensure-e2e-vm: ## Start the e2e VM if it is not already running
	@if ! limactl list 2>/dev/null | grep -q "^$(LIMA_VM)[[:space:]].*Running"; then \
		echo "==> e2e VM not running, starting it"; \
		$(MAKE) start-e2e-vm; \
	else \
		echo "==> e2e VM already running"; \
	fi

copy-e2e-vm: build-e2e-binaries ## Stage e2e/controller sources + pre-built binaries into the VM
	@echo "==> Copying e2e and controller directories into VM"
	limactl shell $(LIMA_VM) -- sudo mkdir -p /root/homelab-ingress/e2e /root/homelab-ingress/controller
	tar czf - --no-xattrs -C $(ROOT)/e2e . | limactl shell $(LIMA_VM) -- sudo tar xzf - -C /root/homelab-ingress/e2e/
	tar czf - --no-xattrs -C $(ROOT)/controller . | limactl shell $(LIMA_VM) -- sudo tar xzf - -C /root/homelab-ingress/controller/
	@echo "==> Copying and installing pre-built binaries"
	tar czf - -C $(BIN_DIR) . | limactl shell $(LIMA_VM) -- sudo tar xzf - -C /tmp/
	limactl shell $(LIMA_VM) -- sudo chmod +x /tmp/ingress /tmp/controller /tmp/oidc-mock
	limactl shell $(LIMA_VM) -- sudo cp /tmp/ingress /usr/local/bin/
	limactl shell $(LIMA_VM) -- sudo cp /tmp/controller /usr/local/bin/
	limactl shell $(LIMA_VM) -- sudo cp /tmp/oidc-mock /usr/local/bin/

test-e2e-bringup: ensure-e2e-vm copy-e2e-vm ## Boot VM and stage sources+binaries without running tests

copy-ingress-vm: ## Stage ingress module sources into the VM (for in-VM unit tests)
	@echo "==> Copying ingress directory into VM"
	limactl shell $(LIMA_VM) -- sudo mkdir -p /root/homelab-ingress/ingress
	tar czf - --no-xattrs -C $(ROOT)/ingress . | limactl shell $(LIMA_VM) -- sudo tar xzf - -C /root/homelab-ingress/ingress/

test-unit-vm: ensure-e2e-vm copy-ingress-vm ## Run ingress unit tests inside the VM (covers linux-only packages)
	@echo "==> Running ingress unit tests inside the VM"
	limactl shell $(LIMA_VM) -- sudo sh -c ' \
		set -e; \
		cd /root/homelab-ingress/ingress && go test ./internal/... -count=1 -v'
	@echo "==> VM left running for reuse (run 'make remove-e2e-vm' to clean up)"

test-e2e-shell: test-e2e-bringup ## Stage everything and drop into an interactive root shell in the VM
	@echo "==> Opening interactive shell (k3s at /etc/rancher/k3s/k3s.yaml, logs in /var/log/hl-ingress/)"
	limactl shell $(LIMA_VM) -- sudo sh -c 'export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; exec bash -l || exec sh -l'

test-e2e-down: remove-e2e-vm ## Alias for remove-e2e-vm

test-e2e: ensure-e2e-vm copy-e2e-vm ## Boot VM (if needed), copy pre-built binaries, run e2e tests
	@echo "==> Running e2e tests inside the VM"
	limactl shell $(LIMA_VM) -- sudo sh -c ' \
		set -e; \
		export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; \
		echo "==> Waiting for k3s node to be Ready"; \
		kubectl wait --for=condition=Ready node --all --timeout=60s; \
		echo "==> Running e2e tests"; \
		cd /root/homelab-ingress/e2e && go test -tags=e2e -timeout=40m -v ./...'
	@echo "==> Stopping VM (best effort)"
	@if [ -z "$(E2E_KEEP_VM)" ]; then \
		$(MAKE) remove-e2e-vm; \
	else \
		echo "==> E2E_KEEP_VM set, leaving VM running (run 'make remove-e2e-vm' to clean up)"; \
	fi
