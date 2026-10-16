# Release checks

Automated tests do not prove that a real Windows audio device, desktop capture, or an internet connection works. Run these checks on Windows 11 before treating this as a usable release.

## Capture and privacy

1. Start playback in two unrelated applications, with distinguishable sounds.
2. Share a monitor. Confirm both sounds reach the viewer, and the selected monitor—not another one—is visible.
3. Stop. Confirm the viewer stops receiving audio and video, and capture/encoding child processes exit.
4. Share one application's window. Confirm its sound is audible and the unrelated application's sound is not.
5. Test another window belonging to the same application. Application audio can span multiple windows; this limitation must remain visible to the broadcaster.
6. Deny/fail application audio capture. Confirm broadcasting fails with an error, without substituting system audio.
7. Speak into a connected microphone with no playback running. Confirm no microphone audio is sent in either sharing mode.
8. Test a minimized/closed target window, target application exit, monitor disconnection, mixed DPI monitors, muted playback, and output-device changes. Confirm errors are visible and stopping still works.

Do not use sensitive windows or private audio for initial testing. System playback can contain notifications and calls. Public viewers can make their own recordings.

## Viewer and relay

1. Open the public URL in current Firefox and Chrome/Edge. Click to enable playback with sound.
2. Join before broadcasting, then start the sender. Confirm the same URL begins playing without a reload.
3. Join midway through a broadcast. Confirm video begins promptly; a missing keyframe must not leave a black screen indefinitely.
4. Stop and restart the sender. Confirm existing viewer pages recover.
5. Disconnect/reconnect the broadcaster's network. Confirm stale publishing sessions eventually release their slot.
6. Open ten viewers. Confirm all receive audio/video; the eleventh receives a capacity message and can recover after another viewer leaves.
7. Leave/rejoin repeatedly. Check server memory, UDP sockets, and goroutines do not grow without bound.
8. Try publishing without a token and with an incorrect token. Confirm both fail, with no secret in responses or logs.
9. While broadcasting, try a second authenticated publisher. Confirm it cannot replace the active stream.
10. Test a network that blocks UDP. Without TURN, failure is expected, but the page must remain responsive and show connection status.

## Latency and deployment

- Put a millisecond stopwatch on the shared screen. Photograph source and viewer displays together and compare several samples. Record median and worst observed delay; target approximately 500 ms, not a guarantee.
- Play a video with visible claps or a synchronized flash/tone. Check audio/video alignment immediately and after at least ten minutes; separate capture clocks must not accumulate drift.
- Measure CPU/GPU use on the sender and outbound traffic on the VPS at ten viewers. Software encoding may be CPU-heavy.
- Verify HTTPS renewal storage is writable by the service and survives restart. Verify the token is not world-readable.
- Confirm only intended TCP/UDP ports are exposed, SSH remains reachable, and DNS does not advertise an unreachable IPv6 address.
- Confirm the packaged FFmpeg supports the required input/encoders and includes its redistribution notices and corresponding-source instructions appropriate to that build.
