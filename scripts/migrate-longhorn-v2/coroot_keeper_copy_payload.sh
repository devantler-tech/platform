#!/bin/bash
set -euo pipefail
export LC_ALL=C
umask 077
original=/original
destination=/destination
staged="$destination/.keeper-copy"
test "$(id -u)" = 65534 && test "$(id -g)" = 65534
test -d "$original/coordination/log" && test -d "$original/coordination/snapshots"
foreign=$(find "$original" -xdev -mindepth 1 -maxdepth 1 ! -name lost+found ! -name coordination -printf x -quit)
test -z "$foreign"
foreign=$(find "$original/coordination" -xdev \( ! -type f ! -type d -o ! -uid 65534 -o ! -gid 65534 -o -type f -links +1 \) -printf x -quit)
test -z "$foreign"
foreign=$(find "$destination" -xdev -mindepth 1 -maxdepth 1 ! -name lost+found -printf x -quit)
test -z "$foreign"
need=$(du -sx --apparent-size --block-size=1 -- "$original/coordination" | cut -f1)
free=$(df -PB1 -- "$destination" | awk 'NR==2 {print $4}')
[[ "$need" =~ ^[0-9]+$ && "$free" =~ ^[0-9]+$ && "$need" -le 10737418240 && "$free" -le 10737418240 ]]
test "$free" -ge "$((need + 67108864))"
mkdir -m 700 -- "$staged"
chmod g-s -- "$staged"
cp -a --one-file-system -- "$original/coordination" "$staged/"
digest() {
  tar --format=ustar --sort=name --numeric-owner --one-file-system -C "$1" -cf - coordination | sha256sum | cut -d ' ' -f1
}
before=$(digest "$original")
after=$(digest "$staged")
[[ "$before" =~ ^[a-f0-9]{64}$ && "$before" = "$after" ]]
sync -f -- "$staged"
mv -- "$staged/coordination" "$destination/coordination"
rmdir -- "$staged"
sync -f -- "$destination"
test "$(digest "$destination")" = "$before"
printf '%s\n' 'COROOT_KEEPER_OFFLINE_COPY=VERIFIED'
