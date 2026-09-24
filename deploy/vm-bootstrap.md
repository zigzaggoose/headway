# VM bootstrap

The commands that built the production VM on 2026-09-24, in the order they ran.
Rebuilding from scratch means running them again; nothing else was done by hand.

**Server:** BinaryLane Standard, Sydney, 1 vCPU, 1 GB, 20 GB, Ubuntu 24.04 LTS,
no backups. Hostname `headway` (the project's first name; the hostname
was never changed), IPv4 `119.42.55.16`.

## 1. SSH key (laptop)

```sh
ssh-keygen -t ed25519 -C "transitlateagain-vm"
ssh-copy-id root@119.42.55.16        # the one use of the emailed root password
ssh root@119.42.55.16 'echo ok'
```

## 2. Key-only SSH

BinaryLane ships `/etc/ssh/sshd_config.d/10-binarylane.conf`. sshd takes the first
value it reads and reads that directory in lexical order, so the override is
`00-`, not a later number.

```sh
cat > /etc/ssh/sshd_config.d/00-transitlateagain.conf <<'CONF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
CONF
sshd -t && systemctl reload ssh
sshd -T | grep -Ei '^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin) '
```

Verified from the laptop: a password-only login is refused with
`Permission denied (publickey)`. BinaryLane's web console still takes the root
password if the key is ever lost.

## 3. Packages, swap, firewall, Docker

```sh
export DEBIAN_FRONTEND=noninteractive
timedatectl set-timezone Australia/Sydney
apt-get update && apt-get -y -o Dpkg::Options::=--force-confold upgrade

# 2 GB swap (§16 q6): the Go build and a schedule load both spike memory.
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
echo 'vm.swappiness=10' > /etc/sysctl.d/99-transitlateagain.conf && sysctl -p /etc/sysctl.d/99-transitlateagain.conf

ufw allow OpenSSH && ufw --force enable

# Docker from Docker's apt repository, not Ubuntu's docker.io.
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

systemctl reboot    # the upgrade brought a new kernel (6.8.0-100)
```

Installed: Docker 29.8.1, Compose v5.5.1.

**ufw does not guard port 8080.** Docker writes its own iptables rules ahead of
ufw's, so a published port is reachable whatever ufw says. The API is public on
purpose; Postgres is not published at all (`deploy/docker-compose.yml` has no
`ports:` for it), which is what actually keeps it private.

## 4. Repository and `.env`

```sh
git clone https://github.com/zigzaggoose/transitlateagain.git /opt/transitlateagain
```

`.env` is written with `umask 077` (mode 0600). The API key is piped from the
laptop's `.env` so it never appears in a terminal or transcript:

```sh
# laptop
grep '^TFNSW_API_KEY=' .env | ssh root@119.42.55.16 'umask 077; cat > /opt/transitlateagain/.env'
# VM
cat >> /opt/transitlateagain/.env <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 24)
RETENTION_DAYS=7
DB_MAX_CONNS=5
HTTP_RATE_LIMIT_RPS=10
EOF
```

Grafana Cloud's remote-write details are appended the same way (step 6). The
token needs the **metrics:write** scope: one generated from the Prometheus
page's read side answers remote write with `401 invalid scope requested`.

`DATABASE_URL` is deliberately absent: Compose builds it from
`POSTGRES_PASSWORD`. Everything else takes the §8 defaults.
`HTTP_RATE_LIMIT_RPS=10` was added after the first deploy: a history query
costs ~20 ms of the one vCPU, so the default of 50 per IP let one client
saturate it, and a person needs one or two a second.

## 5. Start

Every Compose command on the VM layers `deploy/docker-compose.vm.yml`, which
adds port 443, the origin certificate, the client-IP header and Alloy:

```sh
cd /opt/transitlateagain
docker compose --env-file .env -f deploy/docker-compose.yml -f deploy/docker-compose.vm.yml up -d --build
```

## 6. Cloudflare, TLS and Grafana (2026-09-24)

`transitlateagain.dev` is registered at Namecheap with its nameservers set to
Cloudflare (free plan). Cloudflare proxies it to the VM in **Full (strict)**
mode.

**DNS.** One proxied `A` record, `@` → `119.42.55.16`. Adding the site to
Cloudflare imported Namecheap's parking records — an `A` to `192.64.119.71`
and a `www` CNAME to `parkingpage.namecheap.com` — and the first symptom was a
522 with no connection ever reaching the VM. The `A` was corrected and `www`
deleted. The MX and TXT records are Namecheap email forwarding, left alone.

**Origin certificate.** The key is generated on the VM, so it never leaves it;
the CSR goes to Cloudflare (SSL/TLS → Origin Server → "Use my private key and
CSR", 15 years), and the certificate it returns is public:

```sh
install -d -m 0755 /opt/transitlateagain-tls && cd /opt/transitlateagain-tls
openssl ecparam -genkey -name prime256v1 -noout -out origin.key
openssl req -new -key origin.key -out origin.csr -subj "/CN=transitlateagain.dev" \
  -addext "subjectAltName=DNS:transitlateagain.dev,DNS:*.transitlateagain.dev"
chown 65532:65532 origin.key && chmod 0400 origin.key   # the image's nonroot user
cat origin.csr                                             # paste into Cloudflare
cat > origin.pem                                           # paste the certificate, Ctrl-D
chmod 0444 origin.pem
[ "$(openssl x509 -in origin.pem -noout -pubkey)" = "$(openssl pkey -in origin.key -pubout)" ] && echo match
```

It expires on 2041-09-20.

**Grafana Cloud.** Alloy (in `docker-compose.vm.yml`) needs three lines in
`.env`:

```sh
cat >> /opt/transitlateagain/.env   # then paste, Ctrl-D
GRAFANA_REMOTE_WRITE_URL=https://prometheus-prod-41-prod-au-southeast-1.grafana.net/api/prom/push
GRAFANA_USERNAME=3608752
GRAFANA_TOKEN=glc_...
```

Alloy's own counters are the delivery check; its image has no curl:

```sh
docker exec transitlateagain-alloy-1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/12345;
  printf "GET /metrics HTTP/1.0\r\n\r\n" >&3; cat <&3' | grep -E '^prometheus_remote_storage_samples_(total|failed_total)'
```

**Only Cloudflare reaches the API port.** `deploy/firewall.sh` allowlists
Cloudflare's IPv4 ranges on the container port in Docker's `DOCKER-USER`
chain (the script says why ufw cannot), run at every boot after Docker:

```sh
cat > /etc/systemd/system/transitlateagain-firewall.service <<'UNIT'
[Unit]
Description=Transit Late Again: only Cloudflare may reach the API port
Requires=docker.service
After=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh /opt/transitlateagain/deploy/firewall.sh

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload && systemctl enable --now transitlateagain-firewall
iptables -L TRANSITLATEAGAIN-CF -v -n
```

It replaced a first version that capped connections per source IP, which
behind Cloudflare would have throttled Cloudflare's own addresses:

```sh
iptables -D DOCKER-USER -i eth0 -p tcp --dport 8080 -j TRANSITLATEAGAIN-LIMIT
iptables -F TRANSITLATEAGAIN-LIMIT && iptables -X TRANSITLATEAGAIN-LIMIT
```

Checked on 2026-09-24: `https://transitlateagain.dev/readyz` 200 through
Cloudflare's SYD edge; `https://119.42.55.16` and `http://119.42.55.16:8080`
time out from outside; the access log shows Cloudflare as `remote` and the
real client as `client_ip`; 25 concurrent requests from one client through
Cloudflare gave 11 × 200 and 14 × 429. Memory: service 230 MB, Postgres 142
MB, Alloy 79 MB, 232 MB available.

## 7. The rename (2026-09-24, from `headway`)

The steps above use the new names. The VM was built under the old ones and
switched over like this, with the data kept: the Compose file pins the
volume to `headway_pgdata`, and the Postgres role and database are still
`headway`. About a minute without ingest.

```sh
cd /opt/headway && git pull
docker compose --env-file .env -f deploy/docker-compose.yml build   # while the old stack runs
docker compose -p headway --env-file .env -f deploy/docker-compose.yml down --remove-orphans
systemctl disable --now headway-firewall && rm /etc/systemd/system/headway-firewall.service
iptables -D DOCKER-USER -i eth0 -p tcp --dport 8080 -j HEADWAY-LIMIT
iptables -F HEADWAY-LIMIT && iptables -X HEADWAY-LIMIT
mv /opt/headway /opt/transitlateagain && cd /opt/transitlateagain
docker compose --env-file .env -f deploy/docker-compose.yml up -d
# then the unit from step 6, under its new name
```

Checked afterwards: 143,231 observations with the earliest still at 11:53
(131,566 just before), ingest resumed with 0 dropped, `/readyz` 200.

## Updating

```sh
cd /opt/transitlateagain && git pull
docker compose --env-file .env -f deploy/docker-compose.yml -f deploy/docker-compose.vm.yml up -d --build
```

Compose's `stop_grace_period: 45s` lets the old container flush its final batch
before the new one starts.
