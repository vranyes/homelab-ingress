IMAGE_REPO ?= ghcr.io/vranyes/homelab-ingress/controller

.PHONY: all test build push clean

all: test build

test:
	cd router && go test ./internal/... -cover
	cd controller && go test ./internal/... -cover

build:
	KO_DOCKER_REPO=$(IMAGE_REPO) ko build ./controller/cmd/controller/ --bare --platform=linux/arm64,linux/amd64

push:
	KO_DOCKER_REPO=$(IMAGE_REPO) ko build ./controller/cmd/controller/ --bare --platform=linux/arm64,linux/amd64 --push

deploy:
	KO_DOCKER_REPO=$(IMAGE_REPO) ko apply -f controller/deploy/

delete:
	KO_DOCKER_REPO=$(IMAGE_REPO) ko delete -f controller/deploy/

clean:
	rm -f router/router controller/controller
