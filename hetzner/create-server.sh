#!/bin/bash
# Set default values
token=""
server_name=""
image_id=""
server_type="cax21"
location="fsn1"
placement_group="default"
ssh_key_name="ssh-key"

usage() {
  echo "Usage: $0 --token <token> --server-name <server_name> --image-id <image_id> [--server-type <server_type>] [--location <location>] [--placement-group <placement_group>] [--ssh-key-name <ssh_key_name>]"
  echo ""
  echo "Where:"
  echo "  --token <token> is the Hetzner Cloud API token for a Hetzner Cloud project"
  echo "  --server-name <server_name> is the name of the server to create"
  echo "  --image-id <image_id> is the ID of the snapshot image to use for the server"
  echo "  --server-type <server_type> is the type of server to create. Default: cax21"
  echo "  --location <location> is the location to create the server in. Default: fsn1"
  echo "  --placement-group <placement_group> is the name of the placement group to use for the server. Default: default"
  echo "  --ssh-key-name <ssh_key_name> is the name of the SSH key to use for the server. Default: ssh-key"
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
  --server-type)
    server_type="$2"
    shift
    ;;
  --location)
    location="$2"
    shift
    ;;
  --placement-group)
    placement_group="$2"
    shift
    ;;
  --image-id)
    image_id="$2"
    shift
    ;;
  --ssh-key-name)
    ssh_key_name="$2"
    shift
    ;;
  *) usage ;;
  esac
  shift
done

if [ -z "$token" ] || [ -z "$server_name" ] || [ -z "$server_type" ] || [ -z "$location" ] || [ -z "$placement_group" ] || [ -z "$image_id" ] || [ -z "$ssh_key_name" ]; then
  usage
fi

export HCLOUD_TOKEN="$token"

hcloud context create default

hcloud network create --name default --ip-range 10.0.0.0/16

if [ "$(hcloud network describe default | yq -e '.Subnets[]')" == "null" ]; then
  hcloud network add-subnet default --type server --network-zone eu-central
fi

hcloud firewall create --name talos-firewall --rules-file - <<<'[
    {
        "description": "Allow KubeSpan Traffic",
        "direction": "in",
        "port": "51820",
        "protocol": "udp",
        "source_ips": [
            "0.0.0.0/0",
            "::/0"
        ]
    }
]'

hcloud server create --name "$server_name" \
  --type "$server_type" \
  --location "$location" \
  --placement-group "$placement_group" \
  --image "$image_id" \
  --network default \
  --firewall talos-firewall \
  --ssh-key "$ssh_key_name" \
