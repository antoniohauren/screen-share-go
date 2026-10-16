# Screen Share

One Windows broadcaster, one Go relay on Ubuntu, and up to ten browser viewers at a reusable public URL: **https://share.detrouse.com**.

```text
Windows capture → H.264 + Opus → WebRTC → Go relay → WebRTC → browser
```

The server forwards encoded media without transcoding or recording. Its viewer page is embedded in the binary. HTTPS serves the page and signaling; media uses encrypted WebRTC over UDP, not HTTP video streaming.

## Scope

- Windows 11 sender selects a monitor or window. Capture is native, not Firefox screen sharing.
- Monitor sharing includes system playback audio.
- Window sharing includes the owning application's process-tree audio. Other windows belonging to that application can be audible.
- No microphone capture. Failure to capture application audio must never fall back to whole-system audio.
- Public viewing, authenticated broadcasting. Anyone with the viewer URL can watch, hear, and record your stream themselves.
- Initial quality: up to 720p, 30 FPS, constrained-baseline H.264 level 3.1 for browser compatibility. Latency target: roughly 500 ms on a good connection, not a measured guarantee.

## Development

Requires Go 1.26+ and, for the Windows capture helper, a Windows native build toolchain. See [Windows build instructions](windows/README.md).

```sh
go test ./...
go test -race ./...
node --test internal/viewer/state.test.mjs
CGO_ENABLED=0 go build -o bin/screen-share-server ./cmd/server
```

For a local HTTP server:

```sh
(set -euC; umask 077; openssl rand -hex 32 > token)
./bin/screen-share-server -addr 127.0.0.1:8080 -token-file ./token
```

Open `http://localhost:8080`. Use HTTPS for remote deployment. The sender authenticates with the token file's contents; never put this token in the public page or viewer URL.

## Production

See [Ubuntu deployment](deploy/README.md) for DNS, HTTPS, service installation, and firewall configuration. The server needs TCP 80/443 and UDP 50000. Certificate storage and the publisher token are external state/configuration; the application itself is one binary.

Ten viewers at 4 Mbps each need approximately 40 Mbps outbound bandwidth, plus audio and transport overhead. Public access can consume your bandwidth allowance. Stop broadcasting to make the room offline.

## Current limits

- No Linux sender yet.
- No TURN relay fallback: networks that block direct WebRTC UDP may not connect.
- Media transport is IPv4-only; IPv6-only viewers are not supported yet.
- Window capture and audio isolation need verification on actual Windows hardware, including minimized windows, protected content, multi-monitor DPI, and applications that play audio in separate processes.
- Native sender packaging can include FFmpeg and an audio helper beside the executable. Only the server is required to be a single binary.
- Capture correctness, encoder CPU use, real-world latency, and Firefox interoperability must be measured rather than assumed.
