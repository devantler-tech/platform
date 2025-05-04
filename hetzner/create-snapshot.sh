#!/bin/bash
media_path="$1"
token="$2"

if [ -z "$token" ] || [ -z "$media_path" ]; then
  echo "Usage: $0 <media_path> <token>"
  echo ""
  echo "Where:"
  echo "  <media_path> is the path to the media file"
  echo "  <token> is the Hetzner Cloud API token for a Hetzner Cloud project"
  exit 1
fi

export HCLOUD_TOKEN=$2
packer init "$media_path"
packer build "$media_path"
