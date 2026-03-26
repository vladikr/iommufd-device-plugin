ARG TARGETARCH=amd64

FROM --platform=linux/${TARGETARCH} golang:1.22-alpine AS builder

RUN apk add --no-cache ca-certificates

ARG TARGETARCH=amd64
ENV GOARCH=${TARGETARCH}

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /iommufd-device-plugin ./cmd/main.go

FROM --platform=linux/${TARGETARCH} alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /iommufd-device-plugin /iommufd-device-plugin

ENTRYPOINT ["/iommufd-device-plugin"]
CMD ["-log-level=info"]
