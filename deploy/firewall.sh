#!/bin/sh
# Admits only Cloudflare to the API port, run at boot by
# transitlateagain-firewall.service (deploy/vm-bootstrap.md). Idempotent.
#
# The public API is https://transitlateagain.dev, proxied by Cloudflare, which
# absorbs floods and sets CF-Connecting-IP. The rate limiter keys on that
# header, which is only safe if nothing but Cloudflare can connect: anyone
# else could send it and pick their own bucket, and could skip Cloudflare's
# flood protection by using the IP address.
#
# ufw cannot do this: Docker DNATs published ports in its own chains before
# ufw's rules see the packet. DOCKER-USER is the chain Docker reserves for the
# operator and never flushes, and it runs after DNAT, so the port here is the
# container's 8080 (published as 443). Only eth0 is matched, so traffic inside
# the host is untouched. The VM has no public IPv6 address.
#
# Cloudflare's IPv4 ranges, from https://www.cloudflare.com/ips-v4 on
# 2026-09-24. They change rarely; when they do, update this list and rerun.
set -eu

CLOUDFLARE="
173.245.48.0/20
103.21.244.0/22
103.22.200.0/22
103.31.4.0/22
141.101.64.0/18
108.162.192.0/18
190.93.240.0/20
188.114.96.0/20
197.234.240.0/22
198.41.128.0/17
162.158.0.0/15
104.16.0.0/13
104.24.0.0/14
172.64.0.0/13
131.0.72.0/22
"

iptables -N TRANSITLATEAGAIN-CF 2>/dev/null || iptables -F TRANSITLATEAGAIN-CF
for range in $CLOUDFLARE; do
  iptables -A TRANSITLATEAGAIN-CF -s "$range" -j RETURN
done
iptables -A TRANSITLATEAGAIN-CF -j DROP

iptables -C DOCKER-USER -i eth0 -p tcp --dport 8080 -j TRANSITLATEAGAIN-CF 2>/dev/null ||
  iptables -I DOCKER-USER -i eth0 -p tcp --dport 8080 -j TRANSITLATEAGAIN-CF
