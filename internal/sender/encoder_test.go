package sender

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// Optional real encoder smoke test; capture APIs themselves require Windows hardware.
func TestFFmpegRTP(t *testing.T) {
	if os.Getenv("SCREEN_SHARE_FFMPEG_TEST") != "1" {
		t.Skip("set SCREEN_SHARE_FFMPEG_TEST=1 with FFmpeg/libx264/libopus installed")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	for _, video := range []bool{true, false} {
		name := "opus"
		if video {
			name = "h264"
		}
		t.Run(name, func(t *testing.T) {
			sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer sock.Close()
			port := sock.LocalAddr().(*net.UDPAddr).Port
			args, err := audioArgs(port)
			if video {
				args, err = videoArgs(Source{Kind: "monitor", Width: 320, Height: 180}, port)
			}
			if err != nil {
				t.Fatal(err)
			}
			marker := "-vn"
			input := "anullsrc=r=48000:cl=stereo"
			if video {
				marker = "-an"
				input = "testsrc2=size=320x180:rate=30"
			}
			for i, arg := range args {
				if arg == marker {
					args = append([]string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-re", "-f", "lavfi", "-i", input}, args[i:]...)
					break
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, ffmpeg, args...)
			log := &tailLog{}
			cmd.Stderr = log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); cmd.Wait() }()
			sock.SetReadDeadline(time.Now().Add(7 * time.Second))
			buf := make([]byte, 2048)
			for {
				n, _, err := sock.ReadFromUDP(buf)
				if err != nil {
					t.Fatalf("no valid %s RTP: %v\n%s", name, err, log.String())
				}
				packet := &rtp.Packet{}
				if err := packet.Unmarshal(buf[:n]); err != nil {
					t.Fatal(err)
				}
				expected := uint8(111)
				if video {
					expected = 96
				}
				if packet.PayloadType != expected {
					t.Fatalf("payload type %d, want %d", packet.PayloadType, expected)
				}
				if !video {
					if len(packet.Payload) == 0 {
						t.Fatal("empty Opus payload")
					}
					return
				}
				if sps := findSPS(packet.Payload); sps != nil {
					if len(sps) < 4 || sps[1] != 66 || sps[2]&0x40 == 0 || sps[3] != 31 {
						t.Fatalf("not constrained baseline level 3.1: %x", sps)
					}
					return
				}
			}
		})
	}
}

func findSPS(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	if payload[0]&31 == 7 {
		return payload
	}
	if payload[0]&31 != 24 {
		return nil
	}
	for p := payload[1:]; len(p) >= 2; {
		n := int(p[0])<<8 | int(p[1])
		p = p[2:]
		if n > len(p) {
			return nil
		}
		if n > 0 && p[0]&31 == 7 {
			return p[:n]
		}
		p = p[n:]
	}
	return nil
}
