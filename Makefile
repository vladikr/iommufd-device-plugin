BINARY := iommufd-device-plugin
IMAGE ?= quay.io/vladikr/iommufd-device-plugin
TAG ?= latest
ARCH ?= amd64

.PHONY: build image push test clean

build:
	CGO_ENABLED=0 GOARCH=$(ARCH) go build -o $(BINARY) ./cmd/main.go

image:
	podman build --build-arg TARGETARCH=$(ARCH) --platform linux/$(ARCH) -t $(IMAGE):$(TAG)-$(ARCH) .

push:
	podman push $(IMAGE):$(TAG)-$(ARCH)

manifest:
	podman manifest create $(IMAGE):$(TAG)
	podman manifest add $(IMAGE):$(TAG) $(IMAGE):$(TAG)-amd64
	podman manifest add $(IMAGE):$(TAG) $(IMAGE):$(TAG)-arm64

manifest-push:
	podman manifest push $(IMAGE):$(TAG) docker://$(IMAGE):$(TAG)

test:
	go test -v ./...

clean:
	rm -f $(BINARY)
