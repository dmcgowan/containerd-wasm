#!/bin/bash

sudo sed -i 's/archive.ubuntu.com/mirrors.aliyun.com/' /etc/apt/sources.list

# Install Docker
sudo apt-get update
sudo apt-get install -y \
    apt-transport-https \
    ca-certificates \
    curl \
    gnupg-agent \
    software-properties-common
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo apt-key add -
sudo add-apt-repository -y \
   "deb [arch=amd64] https://download.docker.com/linux/ubuntu \
   $(lsb_release -cs) \
   stable"
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io

# Install Golang
sudo add-apt-repository -y ppa:longsleep/golang-backports
sudo apt-get update
sudo apt-get install -y golang-go
echo 'export GOPROXY=https://mirrors.aliyun.com/goproxy/' >> ~/.bashrc 

# Install containerd dependencies
sudo apt-get install -y libseccomp-dev btrfs-tools unzip

# Install gRPC
wget -c https://github.com/google/protobuf/releases/download/v3.5.0/protoc-3.5.0-linux-x86_64.zip
sudo unzip protoc-3.5.0-linux-x86_64.zip -d /usr/local

# Install Wasmer
curl https://get.wasmer.io -sSfL | sh
sudo cp ~/.wasmer/bin/wasmer /usr/local/bin/

# Install Wasmtime
#curl https://wasmtime.dev/install.sh -sSf | bash
#sudo cp ~/.wasmtime/bin/wasmtime /usr/local/bin/

# Setup working direrctory
mkdir -p ~/go/src/github.com/dmcgowan
ln -s /vagrant ~/go/src/github.com/dmcgowan/containerd-wasm


