FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git make

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /loadbalancer ./cmd/loadbalancer/

FROM alpine:3.21

RUN apk add --no-cache nftables iproute2

RUN mkdir -p /var/lib/homelab-lb /etc/homelab-lb

COPY --from=builder /loadbalancer /usr/local/bin/loadbalancer

EXPOSE 50051

ENTRYPOINT ["/usr/local/bin/loadbalancer"]
