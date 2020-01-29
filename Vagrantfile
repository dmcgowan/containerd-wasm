Vagrant.configure("2") do |config|
  config.vm.provider "virtualbox" do |v|
    v.memory = 4096
    v.cpus = 2
  end
  config.vm.box = "ubuntu/bionic64"
  config.vm.provision "shell", path: "init.sh", privileged: false
end
