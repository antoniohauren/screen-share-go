package sender

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
)

// Source comes only from native enumeration, never from browser-supplied geometry/PIDs.
type Source struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Kind                string `json:"kind"`
	X, Y, Width, Height int
	HWND                uintptr
	PID                 uint32
}

func validateSource(s Source) error {
	switch s.Kind {
	case "monitor":
		if s.Width <= 0 || s.Height <= 0 {
			return errors.New("invalid monitor geometry")
		}
	case "window":
		if s.HWND == 0 || s.PID == 0 {
			return errors.New("window has no handle or owning process")
		}
	default:
		return errors.New("unknown capture source")
	}
	return nil
}

func rtpURL(port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", errors.New("invalid RTP port")
	}
	return "rtp://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "?pkt_size=1200", nil
}

func videoArgs(s Source, port int) ([]string, error) {
	if err := validateSource(s); err != nil {
		return nil, err
	}
	u, err := rtpURL(port)
	if err != nil {
		return nil, err
	}
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-probesize", "32", "-analyzeduration", "0", "-f", "gdigrab", "-framerate", "30", "-draw_mouse", "1"}
	if s.Kind == "monitor" {
		a = append(a, "-offset_x", strconv.Itoa(s.X), "-offset_y", strconv.Itoa(s.Y), "-video_size", fmt.Sprintf("%dx%d", s.Width, s.Height), "-i", "desktop")
	} else {
		a = append(a, "-i", fmt.Sprintf("hwnd=0x%x", s.HWND))
	}
	// Fixed level 3.1 matches the relay and common browser receive capabilities.
	return append(a, "-an", "-vf", "scale=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2,format=yuv420p", "-c:v", "libx264", "-profile:v", "baseline", "-level:v", "3.1", "-preset", "ultrafast", "-tune", "zerolatency", "-b:v", "4000k", "-maxrate", "4000k", "-bufsize", "4000k", "-g", "30", "-keyint_min", "30", "-sc_threshold", "0", "-x264-params", "constrained-intra=1:repeat-headers=1", "-payload_type", "96", "-f", "rtp", u), nil
}

func audioArgs(port int) ([]string, error) {
	u, err := rtpURL(port)
	if err != nil {
		return nil, err
	}
	return []string{"-hide_banner", "-loglevel", "warning", "-probesize", "32", "-analyzeduration", "0", "-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0", "-vn", "-c:a", "libopus", "-application", "lowdelay", "-frame_duration", "20", "-b:a", "128k", "-ar", "48000", "-ac", "2", "-payload_type", "111", "-f", "rtp", u}, nil
}

func helperArgs(s Source) ([]string, error) {
	if err := validateSource(s); err != nil {
		return nil, err
	}
	if s.Kind == "window" {
		return []string{"--process", strconv.FormatUint(uint64(s.PID), 10), "--window", strconv.FormatUint(uint64(s.HWND), 10)}, nil
	}
	return []string{"--system"}, nil
}

func executablePaths(dir string) (string, string) {
	return filepath.Join(dir, "ffmpeg.exe"), filepath.Join(dir, "capture-audio.exe")
}
