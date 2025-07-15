packer {
  required_plugins {
    hcloud = {
      version = ">= 1.5.3"
      source  = "github.com/hetznercloud/hcloud"
    }
  }
}

locals {
  image = "talos-v1.10.5-arm64-secureboot/hcloud-arm64-omni-devantler-v1.10.5-secureboot.raw.xz"
}

source "hcloud" "talos" {
  rescue       = "linux64"
  image        = "debian-12" #
  location     = "fsn1" # https://docs.hetzner.com/cloud/general/locations
  server_type  = "cax11" # https://docs.hetzner.com/cloud/servers/overview
  ssh_username = "root"
  snapshot_name = "talos-v1.10.5-arm64-secureboot"
  server_name = "packer-talos-v1.10.5-arm64-secureboot"
}

build {
  sources = ["source.hcloud.talos"]

  provisioner "file" {
    source = "${local.image}"
    destination = "/tmp/talos.raw.xz"
  }

  provisioner "shell" {
    inline = [
      "xz -d -c /tmp/talos.raw.xz | dd of=/dev/sda && sync",
    ]
  }
}
