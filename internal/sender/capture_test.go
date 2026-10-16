package sender

import (
	"reflect"
	"strings"
	"testing"
)

func TestCaptureScope(t *testing.T) {
	monitor := Source{Kind: "monitor", X: -1920, Y: -300, Width: 1920, Height: 1080}
	window := Source{Kind: "window", HWND: 0x1234, PID: 42}
	args, err := videoArgs(monitor, 12345)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-offset_x -1920", "-offset_y -300", "-video_size 1920x1080", "-i desktop", "libx264", "-profile:v baseline", "-level:v 3.1", "scale=1280:720:", "-g 30", "rtp://127.0.0.1:12345?pkt_size=1200"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %q in %s", expected, joined)
		}
	}
	args, err = videoArgs(window, 12345)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "-i hwnd=0x1234") || strings.Contains(joined, "desktop") {
		t.Fatal(joined)
	}
	args, err = helperArgs(window)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"--process", "42", "--window", "4660"}) {
		t.Fatal(args)
	}
	if strings.Contains(strings.Join(args, " "), "--system") {
		t.Fatal("window capture enabled system audio")
	}
	args, err = helperArgs(monitor)
	if err != nil || !reflect.DeepEqual(args, []string{"--system"}) {
		t.Fatal(args, err)
	}
	args, err = audioArgs(12346)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(args, " ")
	for _, expected := range []string{"-f s16le", "-ar 48000", "-ac 2", "-i pipe:0", "libopus", "-payload_type 111", "127.0.0.1:12346"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %s in %s", expected, joined)
		}
	}
	for _, forbidden := range []string{"dshow", "microphone", "wasapi"} {
		if strings.Contains(joined, forbidden) {
			t.Fatal("encoder must consume helper PCM only")
		}
	}
}

func TestInvalidSourceNeverFallsBack(t *testing.T) {
	for _, source := range []Source{{}, {Kind: "microphone"}, {Kind: "monitor", Width: 1}, {Kind: "window", HWND: 1}, {Kind: "window", PID: 2}} {
		if _, err := videoArgs(source, 10000); err == nil {
			t.Error("accepted invalid video source", source)
		}
		if _, err := helperArgs(source); err == nil {
			t.Error("accepted invalid audio source", source)
		}
	}
	for _, port := range []int{-1, 0, 65536} {
		if _, err := audioArgs(port); err == nil {
			t.Error("accepted invalid UDP port", port)
		}
	}
}
