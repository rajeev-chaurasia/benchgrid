# PXE provisioning, with the hardware simulated

A rig that boots from the network has no disk image to keep in step: it gets
its operating system and its agent from a boot server every time it starts,
so every rig of a class runs exactly what the server holds.

`pxe_test.sh` proves that path end to end on a simulated machine. QEMU boots
a diskless x86 VM with network boot first. Its PXE firmware asks QEMU's
built-in DHCP server for an address and a boot file, fetches `boot.ipxe` over
TFTP, and that script fetches a kernel and an initramfs over HTTP. The
initramfs holds busybox, the network driver and a static `rigagent`; its
`/init` brings the network up and starts the agent with its rig id and
control plane address taken from the kernel command line. The test passes
only if that rig registers with a real control plane and completes a
benchmark experiment dispatched to it.

The machine is simulated and the network is QEMU's user mode network. The
protocols are the real ones: DHCP, PXE, TFTP, and HTTP boot through iPXE. CI
runs it on every push.
