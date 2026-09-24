# VM bootstrap

The commands that built the production VM on 2026-09-24, in the order they ran.
Rebuilding from scratch means running them again; nothing else was done by hand.

**Server:** BinaryLane Standard, Sydney, 1 vCPU, 1 GB, 20 GB, Ubuntu 24.04 LTS,
no backups. Hostname `headway`, IPv4 `119.42.55.16`.

## 1. SSH key (laptop)

```sh
ssh-keygen -t ed25519 -C "headway-vm"
ssh-copy-id root@119.42.55.16        # the one use of the emailed root password
ssh root@119.42.55.16 'echo ok'
```

## 2. Key-only SSH

BinaryLane ships `/etc/ssh/sshd_config.d/10-binarylane.conf`. sshd takes the first
value it reads and reads that directory in lexical order, so the override is
`00-`, not a later number.

```sh
cat > /etc/ssh/sshd_config.d/00-headway.conf <<'CONF'
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
echo 'vm.swappiness=10' > /etc/sysctl.d/99-headway.conf && sysctl -p /etc/sysctl.d/99-headway.conf

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
git clone https://github.com/zigzaggoose/headway.git /opt/headway
```

`.env` is written with `umask 077` (mode 0600). The API key is piped from the
laptop's `.env` so it never appears in a terminal or transcript:

```sh
# laptop
grep '^TFNSW_API_KEY=' .env | ssh root@119.42.55.16 'umask 077; cat > /opt/headway/.env'
# VM
cat >> /opt/headway/.env <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 24)
RETENTION_DAYS=7
DB_MAX_CONNS=5
HTTP_RATE_LIMIT_RPS=10
EOF
```

`DATABASE_URL` is deliberately absent: Compose builds it from
`POSTGRES_PASSWORD`. Everything else takes the §8 defaults.
`HTTP_RATE_LIMIT_RPS=10` was added after the first deploy: a history query
costs ~20 ms of the one vCPU, so the default of 50 per IP let one client
saturate it, and a person needs one or two a second.

## 5. Start

```sh
cd /opt/headway
docker compose --env-file .env -f deploy/docker-compose.yml up -d --build
```

## 6. Per-IP connection limits

`deploy/firewall.sh` puts a connection cap and a new-connection rate on port
8080 in Docker's `DOCKER-USER` chain (the script says why ufw cannot), run at
every boot after Docker:

```sh
cat > /etc/systemd/system/headway-firewall.service <<'UNIT'
[Unit]
Description=Headway per-IP limits on the API port
Requires=docker.service
After=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh /opt/headway/deploy/firewall.sh

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload && systemctl enable --now headway-firewall
iptables -L HEADWAY-LIMIT -v -n
```

Measured from a laptop on 2026-09-24: of 120 new connections opened 60 at a
time, the `DROP` rule took 33, and the next request went straight through. 25
concurrent `/v1/lines` requests gave 10 × 200, 10 × 429 from the application
limit and 5 dropped.

## Updating

```sh
cd /opt/headway && git pull
docker compose --env-file .env -f deploy/docker-compose.yml up -d --build
```

Compose's `stop_grace_period: 45s` lets the old container flush its final batch
before the new one starts.
