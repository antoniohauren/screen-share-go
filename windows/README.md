# Windows 11 native sender

`sender.exe` captures through FFmpeg gdigrab and native WASAPI, not browser capture.
It opens a small local browser control panel. Firefox is fine for this panel: it
never calls `getDisplayMedia`. Pick a monitor or a visible, non-minimized window,
enter the publishing token, and press **Start**. **Stop** tears down both media
encoders and the audio helper. Closing the console also kills capture children.
Closing only the browser does **not** stop capture. The token is not persisted.

## Build

Requirements: Windows 11 x64, Go matching root `go.mod`, CMake 3.24+, Visual
Studio 2022 **Desktop development with C++**, and Windows 11 SDK 10.0.22000 or
newer. Use an x64 FFmpeg build with `gdigrab`, `libx264`, `libopus`, and the
`scale` filter. Use a current FFmpeg release supporting gdigrab `hwnd=0x...`;
older releases without HWND selection fail explicitly rather than capture desktop.
No runtime downloads occur. Prefer a static FFmpeg build; dynamic builds require
all their DLLs alongside the executables. Only trusted local binaries belong here.

From PowerShell:

```powershell
.\windows\build.ps1 -FFmpeg C:\tools\ffmpeg\bin\ffmpeg.exe
.\windows\dist\sender.exe
```

Package `sender.exe`, `capture-audio.exe`, `ffmpeg.exe`, and any required FFmpeg
DLLs together in one zip. Include FFmpeg's license, exact build/source information,
and applicable redistribution notices. libx264 builds usually make FFmpeg GPL;
check your chosen distribution's licensing before distributing it. No new Go
dependencies beyond `github.com/pion/webrtc/v4` are needed by this sender.

For Go-only checks on Linux:

```sh
go test ./internal/sender ./cmd/sender
GOOS=windows GOARCH=amd64 go build ./cmd/sender
# Optional: exercise installed FFmpeg encoders and inspect real H.264/Opus RTP.
SCREEN_SHARE_FFMPEG_TEST=1 go test -race ./internal/sender
```

The non-Windows executable reports unsupported platform. Native C++ compilation
and real capture require Windows; a Linux cross-build does not validate WASAPI.

## Capture and isolation

* Monitor selection captures that monitor's exact desktop rectangle, including
  negative virtual-desktop offsets, plus the default **playback** device loopback.
* Window selection captures its HWND and the owning PID's **process tree** audio
  through `ActivateAudioInterfaceAsync` / `INCLUDE_TARGET_PROCESS_TREE`.
  Other windows sharing that tree can therefore be audible. Process ownership,
  not window title, defines audio isolation. Unsupported/denied process capture
  stops the session; it never falls back to whole-system audio.
* No microphone endpoint is opened. Process helper exits when its target exits.
* Audio is clocked signed 16-bit stereo 48 kHz PCM, then Opus stereo 48 kHz,
  128 kbps, 20 ms. Silent playback periods produce silence, not stalled timestamps.
* Video is H.264 constrained baseline level 3.1, yuv420p,
  packetization-mode 1, at most 1280×720, 30 fps, nominal/max 4 Mbps,
  one-second GOP, ultrafast/zerolatency. Smaller sources may be upscaled.
* Encoders send RTP only to ephemeral sockets bound to `127.0.0.1`. Capture
  executables are addressed explicitly next to `sender.exe`, never via PATH or a
   command shell. Children start suspended and enter a Windows kill-on-close job
   before any capture code executes. Stop and sender death kill assigned children.
   Abrupt death in the creation-to-assignment interval can leave an inert suspended
   process (no capture); atomic job-list creation is not exposed by Go's `os/exec`.
* WebRTC default interceptors send RTCP sender reports for both media tracks,
  with the same CNAME in SDP and RTCP. Reports map independent RTP timestamps to
  the sender's common wall clock at packet arrival. This enables receiver clock
  correlation, but does not measure or compensate unequal capture/encoder/pipe
  buffering; actual lip-sync precision still needs Windows hardware testing.
* Local control binds only `127.0.0.1` on a random port, with a 256-bit secret URL,
  exact Host/Origin checks and JSON-only commands. Treat that URL as a local
  control capability. It is printed to the console for manual opening.
* Signaling is non-trickle POST to `https://share.detrouse.com/api/publish` with
  bearer auth. Redirects are refused. Stop closes the peer; DELETE is deliberately
  omitted because this API has no session identity or conditional deletion and
   could otherwise delete another publisher's replacement session.

## Limits / Windows acceptance checklist

Native capture is **not hardware-verified by the Linux development environment**.
Before release, build the helper with the stated SDK and exercise:

1. Two monitors, including one left/above primary; select each independently.
2. Two unrelated apps playing audio. Window mode must include selected app and
   its children, but exclude the other app. Monitor mode includes both.
3. Microphone activity with all playback muted: receiver must hear silence.
4. Pause/resume app audio; confirm A/V sync and silence across idle periods.
5. Bad token, missing FFmpeg/helper, unsupported HWND capture, device removal,
   target process exit: visible error and no remaining capture processes.
6. Run `go test ./internal/sender` on Windows, including job ownership, assignment
   failure and abrupt owner death tests. Repeated Start/Stop, console close and
   abrupt sender termination: check Task Manager for no live capture children.
   Termination exactly before job assignment may leave a suspended orphan as above.
7. Untrusted web origin/Host cannot start or stop via the local HTTP endpoint.

gdigrab is GDI capture: protected content, GPU surfaces, minimized windows, and
some accelerated apps may show black/stale frames. Choose a monitor if window
video is unsupported; doing so deliberately changes audio scope to system playback.
Monitor mode follows the initially selected default playback endpoint; changing
devices may require Stop/Start. Window process audio may span render devices.
No TURN configuration is included; networks blocking direct ICE connectivity may
fail. Viewer must support H.264 and Opus. No Linux native sender is implemented.

API implementation references:
[Microsoft process-loopback sample](https://github.com/microsoft/Windows-classic-samples/tree/main/Samples/ApplicationLoopback),
[FFmpeg gdigrab HWND parser](https://github.com/FFmpeg/FFmpeg/blob/master/libavdevice/gdigrab.c).
