package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const negotiationTimeout = 15 * time.Second

var videoCodec = webrtc.RTPCodecCapability{
	MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
	SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}},
}

var audioCodec = webrtc.RTPCodecCapability{
	MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}

type Config struct {
	Token      string
	UDPPort    int
	PublicIP   string
	MaxViewers int
}

type publisher struct {
	pc        *webrtc.PeerConnection
	video     *webrtc.TrackLocalStaticRTP
	audio     *webrtc.TrackLocalStaticRTP
	videoSSRC atomic.Uint32
	ready     bool // guarded by Relay.mu
	watchers  map[*webrtc.PeerConnection]struct{}
}

// Relay owns one publisher and its viewers. Close releases the shared UDP port.
type Relay struct {
	mu        sync.Mutex
	api       *webrtc.API
	mux       ice.UDPMux
	tokenHash [32]byte
	max       int
	pub       *publisher
	closed    bool
}

func New(cfg Config) (*Relay, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("broadcaster token is required")
	}
	if cfg.UDPPort < 0 || cfg.UDPPort > 65535 {
		return nil, errors.New("UDP port must be between 0 and 65535")
	}
	if cfg.MaxViewers < 1 || cfg.MaxViewers > 10 {
		return nil, errors.New("max viewers must be between 1 and 10")
	}
	if cfg.PublicIP != "" && (net.ParseIP(cfg.PublicIP) == nil || net.ParseIP(cfg.PublicIP).To4() == nil) {
		return nil, errors.New("public IP must be an IPv4 address")
	}
	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: videoCodec, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: audioCodec, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: cfg.UDPPort})
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
	mux := webrtc.NewICEUDPMux(nil, conn)
	settings := webrtc.SettingEngine{}
	settings.SetICEUDPMux(mux)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetICETimeouts(5*time.Second, 10*time.Second, 2*time.Second)
	if cfg.PublicIP != "" {
		settings.SetNAT1To1IPs([]string{cfg.PublicIP}, webrtc.ICECandidateTypeHost)
	}
	return &Relay{
		api: webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(settings)),
		mux: mux, tokenHash: sha256.Sum256([]byte(cfg.Token)), max: cfg.MaxViewers,
	}, nil
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case req.URL.Path == "/api/status" && req.Method == http.MethodGet:
		r.mu.Lock()
		live := r.pub != nil && r.pub.ready
		r.mu.Unlock()
		writeJSON(w, http.StatusOK, struct {
			Live bool `json:"live"`
		}{live})
	case req.URL.Path == "/api/publish" && (req.Method == http.MethodPost || req.Method == http.MethodDelete):
		if !r.authorized(req) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.Method == http.MethodDelete {
			r.mu.Lock()
			p := r.pub
			r.mu.Unlock()
			r.stopPublisher(p)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		r.publish(w, req)
	case req.URL.Path == "/api/watch" && req.Method == http.MethodPost:
		r.watch(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *Relay) authorized(req *http.Request) bool {
	value := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	hash := sha256.Sum256([]byte(value))
	return strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") && subtle.ConstantTimeCompare(hash[:], r.tokenHash[:]) == 1
}

func readOffer(w http.ResponseWriter, req *http.Request) (webrtc.SessionDescription, error) {
	var offer webrtc.SessionDescription
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(&offer); err != nil {
		return offer, errors.New("invalid offer JSON (maximum 1 MiB)")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return offer, errors.New("expected one JSON object")
	}
	if offer.Type != webrtc.SDPTypeOffer || offer.SDP == "" {
		return offer, errors.New("expected SDP offer")
	}
	return offer, nil
}

func (r *Relay) publish(w http.ResponseWriter, req *http.Request) {
	offer, err := readOffer(w, req)
	if err == nil {
		err = checkMediaOffer(offer.SDP, "sendonly")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "peer creation failed", http.StatusInternalServerError)
		return
	}
	video, _ := webrtc.NewTrackLocalStaticRTP(videoCodec, "video", "screen")
	audio, _ := webrtc.NewTrackLocalStaticRTP(audioCodec, "audio", "screen")
	p := &publisher{pc: pc, video: video, audio: audio, watchers: make(map[*webrtc.PeerConnection]struct{})}
	r.mu.Lock()
	if r.closed || r.pub != nil {
		r.mu.Unlock()
		_ = pc.Close()
		http.Error(w, "publisher already active or server closing", http.StatusConflict)
		return
	}
	r.pub = p
	r.mu.Unlock()
	success := false
	defer func() {
		if !success {
			r.stopPublisher(p)
		}
	}()
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			r.mu.Lock()
			if r.pub == p {
				p.ready = true
			}
			r.mu.Unlock()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateClosed:
			r.stopPublisher(p)
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		go func() {
			for {
				if _, _, err := receiver.ReadRTCP(); err != nil {
					return
				}
			}
		}()
		destination := p.audio
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) || !compatibleH264(track.Codec().SDPFmtpLine, "sendonly") {
				r.stopPublisher(p)
				return
			}
			destination = p.video
			p.videoSSRC.Store(uint32(track.SSRC()))
			r.requestKeyframe(p)
		} else if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
			r.stopPublisher(p)
			return
		}
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			// Extension IDs and MID values belong to the publisher's connection.
			packet.Extension = false
			packet.ExtensionProfile = 0
			packet.Extensions = nil
			_ = destination.WriteRTP(packet)
		}
	})
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err = pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			http.Error(w, "transceiver setup failed", http.StatusInternalServerError)
			return
		}
	}
	answer, err := answer(req.Context(), pc, offer)
	if err != nil || !hasMediaDirection(pc, "recvonly") {
		http.Error(w, "publisher negotiation failed", http.StatusBadRequest)
		return
	}
	success = true
	r.expireUnconnected(pc, func() { r.stopPublisher(p) })
	writeJSON(w, http.StatusOK, answer)
}

func (r *Relay) watch(w http.ResponseWriter, req *http.Request) {
	offer, err := readOffer(w, req)
	if err == nil {
		err = checkMediaOffer(offer.SDP, "recvonly")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	p := r.pub
	if p == nil || !p.ready {
		r.mu.Unlock()
		http.Error(w, "publisher not ready", http.StatusServiceUnavailable)
		return
	}
	if len(p.watchers) >= r.max {
		r.mu.Unlock()
		http.Error(w, "viewer limit reached", http.StatusTooManyRequests)
		return
	}
	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		r.mu.Unlock()
		http.Error(w, "peer creation failed", http.StatusInternalServerError)
		return
	}
	p.watchers[pc] = struct{}{}
	r.mu.Unlock()
	remove := func() {
		r.mu.Lock()
		_, exists := p.watchers[pc]
		delete(p.watchers, pc)
		r.mu.Unlock()
		if exists {
			_ = pc.Close()
		}
	}
	success := false
	defer func() {
		if !success {
			remove()
		}
	}()
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			r.requestKeyframe(p)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateClosed:
			remove()
		}
	})
	for _, track := range []*webrtc.TrackLocalStaticRTP{p.video, p.audio} {
		transceiver, addErr := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if addErr != nil {
			http.Error(w, "track setup failed", http.StatusInternalServerError)
			return
		}
		go func(sender *webrtc.RTPSender) {
			for {
				packets, _, err := sender.ReadRTCP()
				if err != nil {
					return
				}
				for _, packet := range packets {
					switch packet.(type) {
					case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
						r.requestKeyframe(p)
					}
				}
			}
		}(transceiver.Sender())
	}
	answer, err := answer(req.Context(), pc, offer)
	if err != nil || !hasMediaDirection(pc, "sendonly") {
		http.Error(w, "viewer negotiation failed; offer must receive H264 and Opus", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	active := r.pub == p && p.ready
	r.mu.Unlock()
	if !active {
		http.Error(w, "publisher stopped", http.StatusServiceUnavailable)
		return
	}
	success = true
	r.expireUnconnected(pc, remove)
	writeJSON(w, http.StatusOK, answer)
}

func answer(ctx context.Context, pc *webrtc.PeerConnection, offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	ctx, cancel := context.WithTimeout(ctx, negotiationTimeout)
	defer cancel()
	if err := pc.SetRemoteDescription(offer); err != nil {
		return nil, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gathered:
		return pc.LocalDescription(), nil
	}
}

func hasMediaDirection(pc *webrtc.PeerConnection, direction string) bool {
	if checkMediaOffer(pc.LocalDescription().SDP, direction) != nil {
		return false
	}
	var desc sdp.SessionDescription
	if err := desc.Unmarshal([]byte(pc.LocalDescription().SDP)); err != nil {
		return false
	}
	video, audio := false, false
	for _, media := range desc.MediaDescriptions {
		if media.MediaName.Port.Value == 0 {
			continue
		}
		if _, ok := media.Attribute(direction); ok {
			video = video || media.MediaName.Media == "video"
			audio = audio || media.MediaName.Media == "audio"
		}
	}
	return video && audio
}

func (r *Relay) expireUnconnected(pc *webrtc.PeerConnection, close func()) {
	time.AfterFunc(negotiationTimeout, func() {
		if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			close()
		}
	})
}

func (r *Relay) requestKeyframe(p *publisher) {
	if ssrc := p.videoSSRC.Load(); ssrc != 0 {
		_ = p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
	}
}

func (r *Relay) stopPublisher(p *publisher) {
	r.mu.Lock()
	if p == nil || r.pub != p {
		r.mu.Unlock()
		return
	}
	r.pub = nil
	watchers := make([]*webrtc.PeerConnection, 0, len(p.watchers))
	for pc := range p.watchers {
		watchers = append(watchers, pc)
		delete(p.watchers, pc)
	}
	r.mu.Unlock()
	for _, pc := range watchers {
		_ = pc.Close()
	}
	_ = p.pc.Close()
}

func (r *Relay) Close() error {
	r.mu.Lock()
	r.closed = true
	p := r.pub
	r.mu.Unlock()
	r.stopPublisher(p)
	return r.mux.Close()
}

func compatibleH264(fmtp, direction string) bool {
	params := make(map[string]string)
	for _, item := range strings.Split(fmtp, ";") {
		key, value, _ := strings.Cut(strings.TrimSpace(item), "=")
		params[strings.ToLower(key)] = strings.ToLower(value)
	}
	profile, err := hex.DecodeString(params["profile-level-id"])
	if params["packetization-mode"] != "1" || err != nil || len(profile) != 3 || profile[0] != 0x42 || profile[1]&0x4f != 0x40 {
		return false
	}
	// Without transcoding, publishers must send level 3.1 and viewers must accept it.
	return profile[2] == 31 || direction == "recvonly" && profile[2] > 31
}

func checkMediaOffer(text, direction string) error {
	var desc sdp.SessionDescription
	if err := desc.Unmarshal([]byte(text)); err != nil {
		return errors.New("invalid SDP")
	}
	video, audio := false, false
	for _, media := range desc.MediaDescriptions {
		if media.MediaName.Media != "video" && media.MediaName.Media != "audio" {
			return errors.New("only one H264 video and one Opus audio track supported")
		}
		if media.MediaName.Port.Value == 0 {
			continue
		}
		opposite := "recvonly"
		if direction == "recvonly" {
			opposite = "sendonly"
		}
		if _, ok := media.Attribute(opposite); ok {
			return fmt.Errorf("offer must support %s media", direction)
		}
		if _, ok := media.Attribute("inactive"); ok {
			return fmt.Errorf("offer must support %s media", direction)
		}
		compatible := false
		for _, attr := range media.Attributes {
			if attr.Key != "rtpmap" {
				continue
			}
			pt, codec, ok := strings.Cut(attr.Value, " ")
			if !ok {
				continue
			}
			if media.MediaName.Media == "audio" && strings.EqualFold(codec, "opus/48000/2") {
				compatible = true
			}
			if media.MediaName.Media == "video" && strings.EqualFold(codec, "H264/90000") {
				for _, params := range media.Attributes {
					if params.Key == "fmtp" && strings.HasPrefix(params.Value, pt+" ") && compatibleH264(strings.TrimPrefix(params.Value, pt+" "), direction) {
						compatible = true
					}
				}
			}
		}
		if !compatible || (media.MediaName.Media == "video" && video) || (media.MediaName.Media == "audio" && audio) {
			return errors.New("offer requires compatible constrained-baseline H264 level 3.1 mode 1 video and Opus audio")
		}
		if media.MediaName.Media == "video" {
			video = true
		} else {
			audio = true
		}
	}
	if !video || !audio {
		return errors.New("offer requires compatible constrained-baseline H264 level 3.1 video and Opus audio")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
