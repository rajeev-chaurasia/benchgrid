#!/usr/bin/env bash
# build_initramfs.sh OUT_DIR RIGAGENT KERNEL_RELEASE
# Packs the initramfs from the host's busybox, the e1000 driver from the
# running kernel's modules, and a static rigagent, and copies the kernel.
set -euo pipefail
out=$1 agent=$2 rel=$3
here=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
mkdir -p "$root/bin" "$root/lib/modules"
cp "$(command -v busybox)" "$root/bin/busybox"
cp "$agent" "$root/bin/rigagent"
cp "$here/udhcpc.sh" "$root/bin/udhcpc.sh"
cp "$here/init" "$root/init"
chmod 0755 "$root/init" "$root/bin/"*
# The generic kernel builds the NIC driver as a module, usually compressed;
# busybox insmod cannot decompress, so it is unpacked here.
mod=$(find "/lib/modules/$rel" -name 'e1000.ko*' | head -1)
case "$mod" in
  *.zst) zstd -dq "$mod" -o "$root/lib/modules/e1000.ko" ;;
  *.xz) xz -dc "$mod" > "$root/lib/modules/e1000.ko" ;;
  *) cp "$mod" "$root/lib/modules/e1000.ko" ;;
esac
mkdir -p "$out"
(cd "$root" && find . | cpio -o -H newc --quiet | gzip -9) > "$out/initramfs.cpio.gz"
sudo cp "/boot/vmlinuz-$rel" "$out/vmlinuz"
sudo chmod 0644 "$out/vmlinuz"
