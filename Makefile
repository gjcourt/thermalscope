IMAGE ?= ghcr.io/gjcourt/thermalscope
TAG   ?= dev

.PHONY: build push test tidy

build:
	docker buildx build --platform=linux/amd64 --load -t $(IMAGE):$(TAG) .

push:
	docker push $(IMAGE):$(TAG)

test:
	go test ./...

tidy:
	go mod tidy
