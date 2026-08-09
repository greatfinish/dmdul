#!/usr/bin/env bash
set -euo pipefail

stage="${1:?usage: capture-stage.sh <NN-stage>}"
case "$stage" in
  [0-9][0-9]-[a-z0-9-]*) ;;
  *) echo "invalid stage name: $stage" >&2; exit 2 ;;
esac

root=/dmdata/DMASM_MIRROR/lab04-rebalance/evidence
out="$root/$stage"
mkdir -p "$out"

{
  date -Ins
  hostname
  uname -a
  printf 'stage=%s\n' "$stage"
} >"$out/host.txt"

: >"$out/headers.txt"
for name in rbln4a rbln4b rbln4c rbln4d rbln4e rbln4r; do
  path="/dev/dmasm/$name"
  {
    printf '\n===== %s =====\n' "$name"
    ls -l "$path" 2>&1 || true
    readlink -f "$path" 2>&1 || true
    if [[ -e "$path" ]]; then
      dev="$(readlink -f "$path")"
      lsblk -b -o NAME,KNAME,PATH,SIZE,TYPE,HCTL,WWN,SERIAL "$dev" 2>&1 || true
      udevadm info --query=property --name="$dev" 2>&1 | sort || true
      blockdev --getsize64 "$dev" 2>&1 || true
      xxd -g 1 -l 256 "$dev" 2>&1 || true
    fi
  } >>"$out/headers.txt"

  if [[ -e "$path" ]]; then
    dd if="$path" of="$out/$name.meta48m.bin" bs=1M count=48 \
      iflag=direct,fullblock status=none
  else
    printf '%s missing at capture time\n' "$path" \
      >"$out/$name.missing.txt"
  fi
done

(
  cd "$out"
  sha256sum ./*.meta48m.bin 2>/dev/null | sort >sha256.txt || true
)

echo "captured $stage in $out"
