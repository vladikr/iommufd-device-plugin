BINARY := iommufd-device-plugin
IMAGE ?= quay.io/vladikr/iommufd-device-plugin
TAG ?= latest

.PHONY: build docker-build docker-push test clean

build:
	CGO_ENABLED=0 go build -o $(BINARY) ./cmd/main.go

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-push:
	docker push $(IMAGE):$(TAG)

test:
	go test -v ./...

clean:
	rm -f $(BINARY)
