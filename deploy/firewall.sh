#!/bin/sh
# Per-source-IP limits on the public API port, run at boot by
# transitlateagain-firewall.service (deploy/vm-bootstrap.md). Idempotent.
#
# ufw cannot do this: Docker DNATs published ports in its own chains before
# ufw's rules see the packet. DOCKER-USER is the chain Docker reserves for
# the operator and never flushes, and it runs after DNAT, so the port here is
# the container's 8080. Only eth0 is matched, so container-to-container and
# SSH-tunnelled traffic is untouched.
#
# The numbers sit well above a person or a front end on keep-alive, which
# opens a handful of connections, and well below what a single machine can
# open. The application's own HTTP_RATE_LIMIT_RPS is the finer limit; this
# drops floods in the kernel, before each connection costs the service a
# goroutine.
set -eu

iptables -N TRANSITLATEAGAIN-LIMIT 2>/dev/null || iptables -F TRANSITLATEAGAIN-LIMIT
# Too many open at once: reset, so a real client fails fast instead of hanging.
iptables -A TRANSITLATEAGAIN-LIMIT -p tcp --syn -m connlimit --connlimit-above 20 --connlimit-mask 32 \
  -j REJECT --reject-with tcp-reset
# Too many new per second: drop, which costs a flooder a retransmit timeout.
iptables -A TRANSITLATEAGAIN-LIMIT -p tcp --syn -m hashlimit --hashlimit-name api8080 \
  --hashlimit-mode srcip --hashlimit-above 20/second --hashlimit-burst 40 -j DROP
iptables -A TRANSITLATEAGAIN-LIMIT -j RETURN

iptables -C DOCKER-USER -i eth0 -p tcp --dport 8080 -j TRANSITLATEAGAIN-LIMIT 2>/dev/null ||
  iptables -I DOCKER-USER -i eth0 -p tcp --dport 8080 -j TRANSITLATEAGAIN-LIMIT
