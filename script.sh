export HCLOUD_TOKEN=$(security find-generic-password -a "$USER" -s "HCLOUD_TOKEN_PROD" -w)
export GHCR_TOKEN=$(security find-generic-password -a "$USER" -s "GITHUB_TOKEN" -w)

while true; do
  output=$(ksail --config ksail.prod.yaml cluster create 2>&1)
  echo "$output"
  if echo "$output" | grep -q "cluster created\|✔.*cluster\|workload.*reconcil"; then
    echo "SUCCESS!"
    break
  fi
  
  if ! echo "$output" | grep -qi "unavailable\|not available\|availability"; then
    echo "UNEXPECTED FAILURE - not an availability issue:"
    echo "$output"
    break
  fi
  
  echo ""
  echo "----------------------------------"
  echo ""  
  sleep 90
done
