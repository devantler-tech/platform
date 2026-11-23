#!/usr/bin/env bash
# Run the benchmark I/O shapes on writable scratch with the production user and no network.
set -euo pipefail
image="${1:?expected the locally built fio image}"
docker run --rm --platform linux/amd64 --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges --user 65532:65532 \
  --tmpfs /data:rw,size=32m,uid=65532,gid=65532 --entrypoint /bin/sh "$image" -ec '
  test "$(fio --version)" = fio-3.41
  fio --name=prepare --filename=/data/io.bin --size=8M --rw=write --bs=1M --ioengine=libaio --direct=1 --end_fsync=1 --output-format=json
  for profile in randread randwrite randrw; do
    for depth in 1 32; do
      fio --name="$profile-$depth-1" --filename=/data/io.bin --size=8M --rw="$profile" --rwmixread=70 --bs=4k --ioengine=libaio --direct=1 --iodepth="$depth" --numjobs=1 --thread=1 --time_based=1 --runtime=1 --randrepeat=1 --randseed=5582 --group_reporting=1 --eta=never --output-format=json
    done
  done
' | jq -se 'length == 7 and all(.[]; .jobs[0].error == 0 and (.jobs[0].read.total_ios + .jobs[0].write.total_ios) > 0)'
