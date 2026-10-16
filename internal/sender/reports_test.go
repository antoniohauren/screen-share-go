package sender

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestSenderReportsShareClockAndCNAME(t *testing.T) {
	api, err := senderAPI()
	if err != nil {
		t.Fatal(err)
	}
	local, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	type result struct {
		kind   webrtc.RTPCodecType
		report *rtcp.SenderReport
		cname  string
	}
	reports := make(chan result, 16)
	remote.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		go func() {
			for {
				if _, _, err := track.ReadRTP(); err != nil {
					return
				}
			}
		}()
		var report *rtcp.SenderReport
		var cname string
		for {
			packets, _, err := receiver.ReadRTCP()
			if err != nil {
				return
			}
			for _, packet := range packets {
				switch p := packet.(type) {
				case *rtcp.SenderReport:
					report = p
				case *rtcp.SourceDescription:
					for _, chunk := range p.Chunks {
						if chunk.Source != uint32(track.SSRC()) {
							continue
						}
						for _, item := range chunk.Items {
							if item.Type == rtcp.SDESCNAME {
								cname = item.Text
							}
						}
					}
				}
			}
			// Pion demultiplexes compound RTCP into separate reads.
			if report != nil && cname != "" {
				select {
				case reports <- result{track.Kind(), report, cname}:
				default:
				}
			}
		}
	})
	tracks := make([]*webrtc.TrackLocalStaticRTP, 2)
	rates := []uint32{90000, 48000}
	origins := []uint32{0xf0000000, 1234567}
	for i, mime := range []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus} {
		cap := webrtc.RTPCodecCapability{MimeType: mime, ClockRate: rates[i]}
		if i == 1 {
			cap.Channels = 2
		}
		tracks[i], err = webrtc.NewTrackLocalStaticRTP(cap, []string{"video", "audio"}[i], streamCNAME)
		if err != nil {
			t.Fatal(err)
		}
		sender, err := local.AddTrack(tracks[i])
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	offer, err := local.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(local)
	if err := local.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gather:
	case <-time.After(5 * time.Second):
		t.Fatal("offer gathering timed out")
	}
	if strings.Count(local.LocalDescription().SDP, "cname:"+streamCNAME) != 2 {
		t.Fatal("missing common SDP CNAME")
	}
	if err := remote.SetRemoteDescription(*local.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := remote.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather = webrtc.GatheringCompletePromise(remote)
	if err := remote.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gather:
	case <-time.After(5 * time.Second):
		t.Fatal("answer gathering timed out")
	}
	if err := local.SetRemoteDescription(*remote.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	// Both independent RTP origins represent the same synthetic acquisition time.
	start := time.Now()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	got := make(map[webrtc.RTPCodecType]result)
	var sequence uint16
	for len(got) < 2 {
		select {
		case now := <-ticker.C:
			sequence++
			for i, track := range tracks {
				packet := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: origins[i] + uint32(now.Sub(start).Seconds()*float64(rates[i])), Marker: true}, Payload: []byte{0x65, 0x01}}
				if err := track.WriteRTP(packet); err != nil {
					t.Fatal(err)
				}
			}
		case report := <-reports:
			if report.cname != streamCNAME || report.report.PacketCount == 0 {
				t.Fatalf("invalid report: %+v", report)
			}
			got[report.kind] = report
		case <-deadline.C:
			t.Fatal("missing audio/video sender reports", got)
		}
	}
	var anchors [2]float64
	for i, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		sr := got[kind].report
		ntpSeconds := float64(sr.NTPTime>>32) + float64(uint32(sr.NTPTime))/4294967296
		anchors[i] = ntpSeconds - float64(uint32(sr.RTPTime-origins[i]))/float64(rates[i])
		// NTP must be current wall time, not an arbitrary per-stream epoch.
		want := float64(start.Unix()) + 2208988800 + float64(start.Nanosecond())/1e9
		if math.Abs(anchors[i]-want) > 0.15 {
			t.Fatalf("incorrect wall-clock mapping: got %.3f want %.3f", anchors[i], want)
		}
	}
	if math.Abs(anchors[0]-anchors[1]) > 0.10 {
		t.Fatalf("audio/video mappings disagree: %v", anchors)
	}
}
