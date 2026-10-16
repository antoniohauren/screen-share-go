package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"screen-share/internal/relay"
)

// Exercise the actual sender codecs against the relay and browser-default codecs.
func TestSenderAndBrowserNegotiateWithRelay(t *testing.T) {
	r, err := relay.New(relay.Config{Token: "test-token", UDPPort: 0, MaxViewers: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	server := httptest.NewServer(r)
	defer server.Close()
	api, err := senderAPI()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	for _, cap := range []webrtc.RTPCodecCapability{
		{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
	} {
		track, err := webrtc.NewTrackLocalStaticRTP(cap, cap.MimeType, streamCNAME)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pub.AddTrack(track); err != nil {
			t.Fatal(err)
		}
	}
	makeOffer := func(pc *webrtc.PeerConnection) webrtc.SessionDescription {
		t.Helper()
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		gathered := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(offer); err != nil {
			t.Fatal(err)
		}
		select {
		case <-gathered:
		case <-time.After(5 * time.Second):
			t.Fatal("ICE gathering timed out")
		}
		return *pc.LocalDescription()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	answer, err := exchange(ctx, server.Client(), server.URL+"/api/publish", "test-token", makeOffer(pub))
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	for {
		response, err := server.Client().Get(server.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		var state struct{ Live bool }
		err = json.NewDecoder(response.Body).Decode(&state)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if state.Live {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("publisher did not become live")
		case <-time.After(10 * time.Millisecond):
		}
	}
	watch, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := watch.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(makeOffer(watch))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Post(server.URL+"/api/watch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("browser negotiation: HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if err := watch.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
}
