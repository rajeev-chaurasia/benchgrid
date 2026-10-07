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
systemctl enable --now chrony || systemctl enable --now chronyd || true

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
systemctl daemon-reload
systemctl enable rigagent
systemctl restart rigagent
