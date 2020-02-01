#!/bin/bash
docker build -t denverdino/containerd-wasm .
id=$(docker create denverdino/containerd-wasm)
docker cp $id:/containerd-shim-wasm-v1 .
docker rm -v $id

