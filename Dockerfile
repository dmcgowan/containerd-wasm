FROM golang:1.13 AS builder
COPY . $GOPATH/src/github.com/dmcgowan/containerd-wasm
WORKDIR $GOPATH/src/github.com/dmcgowan/containerd-wasm/cmd/containerd-shim-wasm-v1
RUN go build .

FROM alpine:3.11
COPY --from=builder /go/src/github.com/dmcgowan/containerd-wasm/cmd/containerd-shim-wasm-v1/containerd-shim-wasm-v1 .



