packer {
  required_plugins {
    hcloud = {
      version = ">= 1.5.3"
      source  = "github.com/hetznercloud/hcloud"
    }
  }
}

locals {
  image = "talos-v1.11.3-amd64/hcloud-amd64-omni-devantler-v1.11.3.raw.xz"
}

source "hcloud" "talos" {
  rescue       = "linux64"
  image        = "debian-13" #
  location     = "fsn1" # https://docs.hetzner.com/cloud/general/locations
  server_type  = "cx23" # https://docs.hetzner.com/cloud/servers/overview
  ssh_username = "root"
  snapshot_name = "talos-v1.11.3-amd64"
  server_name = "packer-talos-v1.11.3-amd64"
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
