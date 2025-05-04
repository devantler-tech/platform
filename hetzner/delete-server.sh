#!/bin/bash
usage() {
  echo "Usage: $0 --token <token> --server-name <server_name>"
  echo ""
  echo "Where:"
  echo "  --token <token> is the Hetzner Cloud API token for a Hetzner Cloud project"
  echo "  --server-name <server_name> is the name of the server to delete"
  exit 1
}

while [[ "$#" -gt 0 ]]; do
  case $1 in
  --token)
    token="$2"
    shift
    ;;
  --server-name)
    server_name="$2"
    shift
    ;;
  *) usage ;;
  esac
  shift
done

if [ -z "$token" ] || [ -z "$server_name" ]; then
  usage
fi

export HCLOUD_TOKEN=$token

hcloud context create talos

hcloud server delete "$server_name"
