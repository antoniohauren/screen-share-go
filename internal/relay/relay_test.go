package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const testToken = "test-broadcaster-secret"

func testRelay(t *testing.T, max int) (*Relay, *httptest.Server) {
	t.Helper()
	r, err := New(Config{Token: testToken, UDPPort: 0, MaxViewers: max})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(r)
	t.Cleanup(func() { server.Close(); _ = r.Close() })
	return r, server
}

func peer(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	engine := &webrtc.MediaEngine{}
	for _, codec := range []struct {
		cap  webrtc.RTPCodecCapability
		pt   webrtc.PayloadType
		kind webrtc.RTPCodecType
	}{{videoCodec, 102, webrtc.RTPCodecTypeVideo}, {audioCodec, 111, webrtc.RTPCodecTypeAudio}} {
		if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: codec.cap, PayloadType: codec.pt}, codec.kind); err != nil {
			t.Fatal(err)
		}
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		t.Fatal(err)
	}
	settings := webrtc.SettingEngine{}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

func publisherPeer(t *testing.T) (*webrtc.PeerConnection, []*webrtc.TrackLocalStaticRTP, <-chan struct{}) {
	t.Helper()
	pc := peer(t)
	feedback := make(chan struct{}, 16)
	var tracks []*webrtc.TrackLocalStaticRTP
	for _, codec := range []webrtc.RTPCodecCapability{videoCodec, audioCodec} {
		track, err := webrtc.NewTrackLocalStaticRTP(codec, codec.MimeType, "publisher")
		if err != nil {
			t.Fatal(err)
		}
		transceiver, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, track)
		go func(sender *webrtc.RTPSender) {
			for {
				packets, _, err := sender.ReadRTCP()
				if err != nil {
					return
				}
				for _, packet := range packets {
					if _, ok := packet.(*rtcp.PictureLossIndication); ok {
						select {
						case feedback <- struct{}{}:
						default:
						}
					}
				}
			}
		}(transceiver.Sender())
	}
	return pc, tracks, feedback
}

func viewerPeer(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	pc := peer(t)
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	return pc
}

func offer(t *testing.T, pc *webrtc.PeerConnection) []byte {
	t.Helper()
	value, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(value); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("ICE gathering timed out")
	}
	data, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func request(t *testing.T, server *httptest.Server, method, path, token string, body []byte, status int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, status, data)
	}
	return data
}

func applyAnswer(t *testing.T, pc *webrtc.PeerConnection, data []byte) {
	t.Helper()
	var answer webrtc.SessionDescription
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Type != webrtc.SDPTypeAnswer || !strings.Contains(answer.SDP, "a=candidate:") {
		t.Fatal("expected non-trickle answer with ICE candidates")
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func live(t *testing.T, server *httptest.Server) bool {
	t.Helper()
	data := request(t, server, http.MethodGet, "/api/status", "", nil, http.StatusOK)
	var result struct{ Live bool }
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result.Live
}

func TestPublishWatchMediaAndLifecycle(t *testing.T) {
	r, server := testRelay(t, 1)
	if live(t, server) {
		t.Fatal("unexpected live publisher")
	}
	pub, tracks, feedback := publisherPeer(t)
	pubOffer := offer(t, pub)
	request(t, server, http.MethodPost, "/api/publish", "wrong", pubOffer, http.StatusUnauthorized)
	request(t, server, http.MethodDelete, "/api/publish", "", nil, http.StatusUnauthorized)
	watch := viewerPeer(t)
	watchOffer := offer(t, watch)
	request(t, server, http.MethodPost, "/api/watch", "", watchOffer, http.StatusServiceUnavailable)
	applyAnswer(t, pub, request(t, server, http.MethodPost, "/api/publish", testToken, pubOffer, http.StatusOK))
	eventually(t, func() bool { return live(t, server) })
	request(t, server, http.MethodPost, "/api/publish", testToken, pubOffer, http.StatusConflict)
	for _, body := range []string{
		strings.ReplaceAll(string(watchOffer), "packetization-mode=1", "packetization-mode=0"),
		strings.ReplaceAll(string(watchOffer), "profile-level-id=42e01f", "profile-level-id=64001f"),
		strings.ReplaceAll(string(watchOffer), "profile-level-id=42e01f", "profile-level-id=42e01e"),
		strings.ReplaceAll(string(watchOffer), "opus/48000/2", "PCMU/8000"),
		strings.ReplaceAll(string(watchOffer), "a=recvonly", "a=sendonly"),
	} {
		request(t, server, http.MethodPost, "/api/watch", "", []byte(body), http.StatusBadRequest)
	}
	request(t, server, http.MethodPost, "/api/watch", "", []byte(`{"type":"offer","sdp":"invalid"}`), http.StatusBadRequest)
	r.mu.Lock()
	if len(r.pub.watchers) != 0 {
		t.Error("failed negotiation retained viewer slot")
	}
	r.mu.Unlock()
	received := make(chan webrtc.RTPCodecType, 2)
	watch.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		packet, _, err := track.ReadRTP()
		if err != nil || len(packet.Payload) == 0 {
			return
		}
		if packet.Extension || len(packet.Extensions) != 0 {
			t.Error("publisher RTP extensions reached viewer")
		}
		received <- track.Kind()
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			_ = watch.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
		}
	})
	answer := request(t, server, http.MethodPost, "/api/watch", "", watchOffer, http.StatusOK)
	applyAnswer(t, watch, answer)
	request(t, server, http.MethodPost, "/api/watch", "", watchOffer, http.StatusTooManyRequests)
	eventually(t, func() bool { return watch.ConnectionState() == webrtc.PeerConnectionStateConnected })
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		var sequence uint16
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sequence++
				for i, track := range tracks {
					packet := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: uint32(sequence) * 960, SSRC: uint32(i + 1)}, Payload: []byte{0x65, 0x01, 0x02}}
					_ = packet.SetExtension(14, []byte("publisher-mid"))
					_ = track.WriteRTP(packet)
				}
			}
		}
	}()
	t.Cleanup(func() { close(done); <-stopped })
	kinds := make(map[webrtc.RTPCodecType]bool)
	for len(kinds) < 2 {
		select {
		case kind := <-received:
			kinds[kind] = true
		case <-time.After(5 * time.Second):
			t.Fatal("audio/video RTP not forwarded")
		}
	}
	select {
	case <-feedback:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher received no PLI feedback")
	}
	r.mu.Lock()
	old := r.pub
	var serverWatch *webrtc.PeerConnection
	for pc := range old.watchers {
		serverWatch = pc
	}
	r.mu.Unlock()
	request(t, server, http.MethodDelete, "/api/publish", "wrong", nil, http.StatusUnauthorized)
	request(t, server, http.MethodDelete, "/api/publish", testToken, nil, http.StatusNoContent)
	if live(t, server) || serverWatch.ConnectionState() != webrtc.PeerConnectionStateClosed {
		t.Fatal("stopping publisher did not stop viewers")
	}
	r.mu.Lock()
	if len(old.watchers) != 0 {
		t.Error("stopping publisher retained viewers")
	}
	r.mu.Unlock()
	newPub, _, _ := publisherPeer(t)
	applyAnswer(t, newPub, request(t, server, http.MethodPost, "/api/publish", testToken, offer(t, newPub), http.StatusOK))
	eventually(t, func() bool { return live(t, server) })
	_ = newPub.Close()
	eventually(t, func() bool { return !live(t, server) })
}

func TestConcurrentPublisherReservation(t *testing.T) {
	_, server := testRelay(t, 10)
	p1, _, _ := publisherPeer(t)
	p2, _, _ := publisherPeer(t)
	offers := [][]byte{offer(t, p1), offer(t, p2)}
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, data := range offers {
		wg.Add(1)
		go func(data []byte) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/publish", bytes.NewReader(data))
			req.Header.Set("Authorization", "Bearer "+testToken)
			res, err := server.Client().Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
			statuses <- res.StatusCode
		}(data)
	}
	wg.Wait()
	a, b := <-statuses, <-statuses
	if !((a == 200 && b == 409) || (a == 409 && b == 200)) {
		t.Fatalf("concurrent publish statuses: %d, %d", a, b)
	}
}

func TestTenViewerFanout(t *testing.T) {
	_, server := testRelay(t, 10)
	pub, tracks, _ := publisherPeer(t)
	applyAnswer(t, pub, request(t, server, http.MethodPost, "/api/publish", testToken, offer(t, pub), http.StatusOK))
	eventually(t, func() bool { return live(t, server) })
	received := make(chan struct{}, 20)
	for range 10 {
		watch := viewerPeer(t)
		watch.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			if _, _, err := track.ReadRTP(); err == nil {
				received <- struct{}{}
			}
		})
		applyAnswer(t, watch, request(t, server, http.MethodPost, "/api/watch", "", offer(t, watch), http.StatusOK))
		eventually(t, func() bool { return watch.ConnectionState() == webrtc.PeerConnectionStateConnected })
	}
	extra := viewerPeer(t)
	request(t, server, http.MethodPost, "/api/watch", "", offer(t, extra), http.StatusTooManyRequests)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var sequence uint16
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sequence++
				for i, track := range tracks {
					_ = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: uint32(sequence) * 960, SSRC: uint32(i + 1)}, Payload: []byte{0x65, 0x01, 0x02}})
				}
			}
		}
	}()
	t.Cleanup(func() { close(done); <-stopped })
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for range 20 {
		select {
		case <-received:
		case <-deadline.C:
			t.Fatal("not all ten viewers received both audio and video")
		}
	}
}

func TestUnconnectedPublisherExpires(t *testing.T) {
	r, server := testRelay(t, 1)
	pc, _, _ := publisherPeer(t)
	request(t, server, http.MethodPost, "/api/publish", testToken, offer(t, pc), http.StatusOK)
	// A client that never applies the answer must not retain the publisher slot.
	deadline := time.Now().Add(negotiationTimeout + 3*time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		empty := r.pub == nil
		r.mu.Unlock()
		if empty {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("unconnected publisher retained its slot")
}

func TestValidation(t *testing.T) {
	for _, cfg := range []Config{
		{UDPPort: 0, MaxViewers: 1},
		{Token: "token", UDPPort: -1, MaxViewers: 1},
		{Token: "token", UDPPort: 65536, MaxViewers: 1},
		{Token: "token", UDPPort: 0, MaxViewers: 0},
		{Token: "token", UDPPort: 0, MaxViewers: 11},
		{Token: "token", UDPPort: 0, MaxViewers: 1, PublicIP: "bad-ip"},
	} {
		if r, err := New(cfg); err == nil {
			_ = r.Close()
			t.Errorf("accepted invalid config: %+v", cfg)
		}
	}
	_, server := testRelay(t, 1)
	for _, body := range []string{`{}`, `{"type":"answer","sdp":"x"}`, `{"type":"offer","sdp":"invalid"}`, `{} {}`, strings.Repeat("x", (1<<20)+1)} {
		request(t, server, http.MethodPost, "/api/publish", testToken, []byte(body), http.StatusBadRequest)
	}
	pc, _, _ := publisherPeer(t)
	data := offer(t, pc)
	var desc webrtc.SessionDescription
	_ = json.Unmarshal(data, &desc)
	for _, bad := range []string{
		strings.ReplaceAll(desc.SDP, "packetization-mode=1", "packetization-mode=0"),
		strings.ReplaceAll(desc.SDP, "profile-level-id=42e01f", "profile-level-id=64001f"),
		strings.ReplaceAll(desc.SDP, "profile-level-id=42e01f", "profile-level-id=42e028"),
		strings.ReplaceAll(desc.SDP, "a=sendonly", "a=recvonly"),
		strings.ReplaceAll(desc.SDP, "opus/48000/2", "PCMU/8000"),
	} {
		if err := checkMediaOffer(bad, "sendonly"); err == nil {
			t.Error("accepted incompatible publisher SDP")
		}
	}
}

func TestH264Compatibility(t *testing.T) {
	for _, tc := range []struct {
		profile, direction string
		want               bool
	}{
		{"42e01f", "sendonly", true},
		{"42c01f", "sendonly", true},
		{"42e028", "sendonly", false},
		{"42e01e", "sendonly", false},
		{"42e01f", "recvonly", true},
		{"42e028", "recvonly", true},
		{"42e01e", "recvonly", false},
		{"42001f", "recvonly", false},
		{"64001f", "recvonly", false},
		{"garbage", "recvonly", false},
	} {
		fmtp := "packetization-mode=1;profile-level-id=" + tc.profile
		if got := compatibleH264(fmtp, tc.direction); got != tc.want {
			t.Errorf("%s %s: got %v, want %v", tc.profile, tc.direction, got, tc.want)
		}
	}
}
