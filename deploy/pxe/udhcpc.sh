#!/bin/sh
# Applies the lease udhcpc gets, which busybox leaves to a script.
case "$1" in
bound|renew)
  ip addr flush dev "$interface"
  ip addr add "$ip/${mask:-24}" dev "$interface"
  [ -n "$router" ] && ip route add default via "${router%% *}"
  ;;
esac
