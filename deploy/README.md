# Ubuntu deployment

One Go server binary; systemd runs it as an unprivileged, nonlogin user. HTTPS
uses built-in autocert for `share.detrouse.com`; no reverse proxy required.
These are manual instructions, not evidence that DNS or this host is ready.

## DNS and network

- Set `share.detrouse.com` A to this server's public IPv4 address.
- Use an A record for this version. WebRTC media is IPv4-only, so viewers need
  IPv4 connectivity. Omit AAAA unless HTTPS/ACME over IPv6 is independently
  working; publishing AAAA does not enable IPv6 media. Remove stale or unreachable
  AAAA records: browsers and ACME validation may prefer IPv6.
- Forward those ports through any NAT and permit them in provider firewalls.
  TCP 80 must remain reachable for ACME HTTP-01 issuance and renewals; TCP 443
  serves HTTPS. UDP 50000 carries WebRTC media. Check for conflicting listeners;
  do not stop an existing website merely to free these ports.
- Verify the DNS records and routing yourself before starting the service.

If the VPS has a private interface behind provider NAT, add
`-public-ip YOUR_PUBLIC_IPV4` to the service's `ExecStart`. This changes advertised
ICE addresses, not firewall rules or port forwarding. Use the numeric IPv4
address, not the domain name.

If an existing web server owns TCP 80/443, use it to terminate HTTPS for
`share.detrouse.com` and reverse-proxy all paths to `127.0.0.1:8080`. Before
installing the supplied unit, change its `ExecStart` to use
`-addr 127.0.0.1:8080 -token-file /etc/screen-share/token -udp-port 50000`,
omitting `-domain` and `-cert-dir`. Remove the unit's `AmbientCapabilities` and
`CapabilityBoundingSet` lines; privileged port binding is no longer needed.
Certificate management belongs to that existing web server in this setup.
UDP 50000 must still reach the Go process directly; an HTTP reverse proxy does
not relay WebRTC media. Allow at least 30 seconds for signaling requests.

## Build and install

Install the current stable Go release from <https://go.dev/doc/install> (Ubuntu's
packaged version may lag), plus `openssl`. From the repository root, build the
server main package with CGO disabled to avoid shared-library dependencies:

```sh
go version
CGO_ENABLED=0 go build -trimpath -o screen-share-server ./cmd/server

sudo useradd --system --user-group --home-dir /var/lib/screen-share --no-create-home --shell /usr/sbin/nologin screen-share
sudo install -o root -g root -m 0755 screen-share-server /usr/local/bin/screen-share-server
sudo install -d -o root -g screen-share -m 0750 /etc/screen-share
```

If the dedicated user already exists, verify its group and nonlogin shell rather
than recreating it. Create the broadcaster secret without displaying it, putting
it in shell history, or passing it as a process argument:

```sh
sudo sh -c 'set -euC; umask 027; openssl rand -hex 32 > /etc/screen-share/token; chown root:screen-share /etc/screen-share/token; chmod 0640 /etc/screen-share/token'
```

The command refuses to overwrite an existing token. Do not enable shell tracing
or print the token into terminals, journals, chat, screenshots, or deployment
logs. Transfer it to the authorized broadcaster using a private, trusted secret
channel; keep it out of public URLs. For rotation, stop the service, replace the
file securely with a fresh secret and the same ownership/mode, then restart.

Preserve remote access **before** enabling UFW. Use your actual SSH port below
(22 is only the default), and retain any other rules this host needs:

```sh
sudo ufw allow 22/tcp
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 50000/udp
sudo ufw status verbose
sudo ufw enable
```

For published AAAA records, ensure UFW IPv6 support is enabled too. Keep an
existing SSH session open and test a second connection before disconnecting.

```sh
sudo install -o root -g root -m 0644 deploy/screen-share.service /etc/systemd/system/screen-share.service
sudo systemd-analyze verify /etc/systemd/system/screen-share.service
sudo systemctl daemon-reload
sudo systemctl enable --now screen-share.service
sudo systemctl status screen-share.service
sudo journalctl -u screen-share.service -n 50 --no-pager
```

systemd creates writable `/var/lib/screen-share`; autocert stores persistent
certificates under its `certs` subdirectory. The rest of the filesystem is
read-only to the service, home directories are hidden, and only the privileged
port-binding capability is granted. Networking is not isolated: ACME needs
outbound DNS/HTTPS and inbound TCP 80, while media needs UDP 50000. `AF_NETLINK`
allows Go to enumerate local interfaces for ICE candidate gathering.

For local HTTP development, use `-addr 127.0.0.1:8080 -token-file ./token` with a
local secret file. The unit explicitly selects HTTPS port 443 and the production
domain, certificate directory, token file, and UDP port.

## Sharing and limits

- Reusable public viewer URL: **https://share.detrouse.com/**. Share the same URL
  across broadcasts; viewers do not receive the broadcaster secret.
- Only the authorized broadcaster supplies the token through the broadcaster
  interface. The server must validate it; possession authorizes broadcasting.
- Viewing is public, not private or access-controlled. Anyone with the URL can
  watch an active stream; hide sensitive windows and notifications before sharing.
- The server does not record streams. Viewers can still capture or record locally.
- Budget roughly **40 Mbps server outbound for 10 viewers at 4 Mbps each**, plus
  protocol overhead; actual usage depends on stream bitrate and viewer count.
- TURN is deferred. Restrictive corporate/mobile networks, blocked UDP, or some
  NAT configurations may prevent media even when HTTPS loads successfully.

After installation, test HTTPS and a real broadcaster/viewer pair from different
networks. Confirm unauthorized broadcasting is rejected and verify media flows;
an HTTP health check alone cannot establish WebRTC connectivity.
