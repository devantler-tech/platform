packer {
  required_plugins {
    hcloud = {
      version = ">= 1.5.3"
      source  = "github.com/hetznercloud/hcloud"
    }
  }
}

locals {
  image = "hcloud-amd64-omni-devantler-1.12.5-0c667e.xz"
}

source "hcloud" "talos" {
  rescue       = "linux64"
  image        = "debian-13"
  location     = "fsn1"
  server_type  = "cx23"
  ssh_username = "root"
  snapshot_name = "talos-v1.12.5-amd64"
  server_name = "packer-talos-v1.12.5-amd64"
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
