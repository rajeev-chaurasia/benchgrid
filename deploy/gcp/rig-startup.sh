#!/usr/bin/env bash
# Startup script for a bench node, run by the guest agent on every boot. It
# is idempotent: the first boot tunes the kernel and reboots, later boots
# apply the runtime settings and (re)start the agent. Settings come from
# instance metadata, so one script serves tuned and untuned nodes.
set -euo pipefail
md() { curl -fsS -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" 2>/dev/null || true; }
TUNED=$(md benchgrid-tuned)
BENCH_CPUS=$(md benchgrid-bench-cpus)
CLASS=$(md benchgrid-class)
CONTROL=$(md benchgrid-control)
AGENT_URL=$(md benchgrid-agent)

# Isolate the bench CPU from the scheduler, timer ticks and RCU callbacks,
# and steer interrupts away from it. These are boot parameters, so the first
# boot sets them and reboots once.
if [ "$TUNED" = "true" ] && ! grep -q "isolcpus=$BENCH_CPUS" /proc/cmdline; then
  cat >/etc/default/grub.d/99-benchgrid.cfg <<CFG
GRUB_CMDLINE_LINUX_DEFAULT="\$GRUB_CMDLINE_LINUX_DEFAULT isolcpus=$BENCH_CPUS nohz_full=$BENCH_CPUS rcu_nocbs=$BENCH_CPUS irqaffinity=0"
CFG
  update-grub
  reboot
  exit 0
fi

export DEBIAN_FRONTEND=noninteractive
command -v chronyc >/dev/null || { apt-get update -qq && apt-get install -y -qq chrony; }
# Google's metadata server is the time source GCE recommends: a hop away,
# with no NAT in between. Debian's default pool servers are reached through
# NAT and were measured, on a rig like this, at 53 ms off with a 13 s root
# dispersion. makestep lets chrony step rather than slew after boot.
conf=/etc/chrony/chrony.conf
[ -f /etc/chrony.conf ] && conf=/etc/chrony.conf
if ! grep -q "^server metadata.google.internal" "$conf"; then
  sed -i -e 's/^\(pool .*\)/# \1/' -e 's/^\(server .*\)/# \1/' -e 's/^\(sourcedir .*\)/# \1/' "$conf"
  printf 'server metadata.google.internal prefer iburst\nmakestep 0.1 3\n' >> "$conf"
fi
systemctl enable chrony 2>/dev/null || systemctl enable chronyd 2>/dev/null || true
systemctl restart chrony 2>/dev/null || systemctl restart chronyd 2>/dev/null || true
# The agent must not start, and so must not advertise the rig, until the
# clock is trustworthy by chrony's own full error bound: |offset| plus half
# the root delay plus the root dispersion, under 5 ms. A fresh node that
# joined before this check once advertised itself with a 62 second bound.
for _ in $(seq 1 120); do
  bound=$(chronyc -c tracking 2>/dev/null | awk -F, '{o=$5; if (o < 0) o = -o; printf "%d", (o + $11 / 2 + $12) * 1000000}')
  [ -n "$bound" ] && [ "$bound" -lt 5000 ] && break
  sleep 5
done

if [ "$TUNED" = "true" ]; then
  echo never > /sys/kernel/mm/transparent_hugepage/enabled
  systemctl stop irqbalance 2>/dev/null || true
  for f in /proc/irq/*/smp_affinity_list; do echo 0 > "$f" 2>/dev/null || true; done
fi

# On a GPU node the driver may still be installing on first boot. The agent
# describes its hardware once, at start, so it must not start before
# nvidia-smi can see the GPU, or it would advertise a rig with no GPU.
if [ "$CLASS" = "gcp-t4" ]; then
  for _ in $(seq 1 120); do nvidia-smi >/dev/null 2>&1 && break; sleep 5; done
fi

gcloud storage cp "$AGENT_URL" /usr/local/bin/rigagent.new
chmod 0755 /usr/local/bin/rigagent.new
mv /usr/local/bin/rigagent.new /usr/local/bin/rigagent

IP=$(curl -fsS -H 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/ip)
PIN=""
[ "$TUNED" = "true" ] && PIN="-bench-cpus $BENCH_CPUS -cgroups"
mkdir -p /etc/benchgrid
cat >/etc/systemd/system/rigagent.service <<UNIT
[Unit]
Description=benchgrid rig agent
Wants=network-online.target chrony.service
After=network-online.target

[Service]
ExecStart=/usr/local/bin/rigagent -id $(hostname) -state-dir /var/lib/benchgrid -control $CONTROL -listen :9090 -endpoint http://$IP:9090 -hardware-class $CLASS $PIN
StateDirectory=benchgrid
Delegate=yes
KillMode=process
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
# A stand-in for a co-located service: when the instance's benchgrid-noise
# metadata is "on", core 0 is kept busy in bursts of 0.3 to 1.5 seconds with
# gaps of the same range. Every node runs the same noise. On a tuned node,
# core 0 is the system core and the benchmark is isolated from it; on a
# default node, the benchmark can be scheduled onto it. The isolation study
# turns it on and off for the whole fleet at once.
cat >/usr/local/bin/benchgrid-noise <<'NOISE'
#!/bin/bash
md='http://metadata.google.internal/computeMetadata/v1/instance/attributes/benchgrid-noise'
secs() { printf '%d.%03d' $(($1 / 1000)) $(($1 % 1000)); }
while true; do
  if [ "$(curl -fsS -H 'Metadata-Flavor: Google' "$md" 2>/dev/null)" = on ]; then
    on=$(( (RANDOM % 1200) + 300 )) off=$(( (RANDOM % 1200) + 300 ))
    timeout "$(secs $on)" taskset -c 0 bash -c 'while :; do :; done' || true
    sleep "$(secs $off)"
  else
    sleep 2
  fi
done
NOISE
chmod 0755 /usr/local/bin/benchgrid-noise
cat >/etc/systemd/system/benchgrid-noise.service <<UNIT
[Unit]
Description=benchgrid noise source for the isolation study

[Service]
ExecStart=/usr/local/bin/benchgrid-noise
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable rigagent benchgrid-noise
systemctl restart rigagent benchgrid-noise
