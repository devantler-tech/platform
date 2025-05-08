#!/bin/bash
# Set default values
token=""
media_path=""

usage() {
  echo "Usage: $0 --media-path <media_path> --token <token>"
  echo ""
  echo "Where:"
  echo "  --token <token> is the Hetzner Cloud API token for a Hetzner Cloud project"
  echo "  --media-path <media_path> is the path to the media file"
  exit 1
}

while [[ "$#" -gt 0 ]]; do
  case $1 in
  --token)
    token="$2"
    shift
    ;;
  --media-path)
    media_path="$2"
    shift
    ;;
  *) usage ;;
  esac
  shift
done

if [ -z "$media_path" ] || [ -z "$token" ]; then
  usage
fi

export HCLOUD_TOKEN=$token
packer init "$media_path"
packer build "$media_path"
