package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

const publishURL = "https://share.detrouse.com/api/publish"
const streamCNAME = "native"

// Match Pion's SDP CNAME (TrackLocal.StreamID) in outgoing compound RTCP too.
type reportCNAME struct{ interceptor.NoOp }

func (r reportCNAME) NewInterceptor(string) (interceptor.Interceptor, error) { return &r, nil }

func (r reportCNAME) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(packets []rtcp.Packet, attrs interceptor.Attributes) (int, error) {
		var chunks []rtcp.SourceDescriptionChunk
		for _, packet := range packets {
			if report, ok := packet.(*rtcp.SenderReport); ok {
				chunks = append(chunks, rtcp.SourceDescriptionChunk{Source: report.SSRC, Items: []rtcp.SourceDescriptionItem{{Type: rtcp.SDESCNAME, Text: streamCNAME}}})
			}
		}
		if len(chunks) != 0 {
			packets = append(packets, &rtcp.SourceDescription{Chunks: chunks})
		}
		return writer.Write(packets, attrs)
	})
}

type tailLog struct {
	mu   sync.Mutex
	text string
}

func (b *tailLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text += string(p)
	if len(b.text) > 8192 {
		b.text = b.text[len(b.text)-8192:]
	}
	return len(p), nil
}
func (b *tailLog) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.text }

func exchange(ctx context.Context, client *http.Client, endpoint, token string, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	var answer webrtc.SessionDescription
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return answer, errors.New("invalid publishing token")
	}
	body, err := json.Marshal(offer)
	if err != nil {
		return answer, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return answer, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// Never forward the bearer token to a redirect destination.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return answer, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return answer, fmt.Errorf("publish rejected: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return answer, fmt.Errorf("invalid answer: %w", err)
	}
	if answer.Type != webrtc.SDPTypeAnswer || answer.SDP == "" {
		return answer, errors.New("server returned no SDP answer")
	}
	return answer, nil
}

func senderAPI() (*webrtc.API, error) {
	media := &webrtc.MediaEngine{}
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, PayloadType: 96}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1"}, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	registry := &interceptor.Registry{}
	registry.Add(reportCNAME{})
	// Sender reports map both RTP clocks to time.Now at packet arrival, not capture time.
	if err := webrtc.RegisterDefaultInterceptors(media, registry); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(media), webrtc.WithInterceptorRegistry(registry)), nil
}

func runSession(ctx context.Context, source Source, token, dir string, ready func()) error {
	if err := validateCurrentSource(source); err != nil {
		return err
	}
	ffmpeg, helper := executablePaths(dir)
	for _, p := range []string{ffmpeg, helper} {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return fmt.Errorf("required executable missing: %s", p)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	group, err := newProcessGroup()
	if err != nil {
		return err
	}
	defer group.close()
	api, err := senderAPI()
	if err != nil {
		return err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer pc.Close()
	fail := make(chan error, 8)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed {
			select {
			case fail <- errors.New("WebRTC connection failed"):
			default:
			}
		}
	})
	ports := make([]int, 2)
	for i, mime := range []string{webrtc.MimeTypeH264, webrtc.MimeTypeOpus} {
		cap := webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000}
		if i == 1 {
			cap.ClockRate = 48000
			cap.Channels = 2
		}
		track, err := webrtc.NewTrackLocalStaticRTP(cap, []string{"video", "audio"}[i], streamCNAME)
		if err != nil {
			return err
		}
		sender, err := pc.AddTrack(track)
		if err != nil {
			return err
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buf); err != nil {
					return
				}
			}
		}()
		sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return err
		}
		defer sock.Close()
		ports[i] = sock.LocalAddr().(*net.UDPAddr).Port
		go func() {
			buf := make([]byte, 2048)
			for {
				n, _, err := sock.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if _, err = track.Write(buf[:n]); err != nil && ctx.Err() == nil {
					select {
					case fail <- err:
					default:
					}
					return
				}
			}
		}()
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gather:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return errors.New("ICE gathering timed out")
	}
	answer, err := exchange(ctx, &http.Client{Timeout: 25 * time.Second}, publishURL, token, *pc.LocalDescription())
	if err != nil {
		return err
	}
	if err = pc.SetRemoteDescription(answer); err != nil {
		return err
	}
	// Signaling may take seconds; never capture a stale/reassigned selection.
	if err = validateCurrentSource(source); err != nil {
		return err
	}
	// DELETE is deliberately omitted: this API has no session ID / conditional delete.
	// Closing this peer cannot delete a replacement publisher's session.
	vargs, err := videoArgs(source, ports[0])
	if err != nil {
		return err
	}
	aargs, err := audioArgs(ports[1])
	if err != nil {
		return err
	}
	hargs, err := helperArgs(source)
	if err != nil {
		return err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()
	video := exec.CommandContext(ctx, ffmpeg, vargs...)
	audio := exec.CommandContext(ctx, ffmpeg, aargs...)
	audio.Stdin = reader
	capture := exec.CommandContext(ctx, helper, hargs...)
	capture.Stdout = writer
	var workers sync.WaitGroup
	// Cancel before waiting; all child processes also belong to a kill-on-close job.
	defer func() { cancel(); group.close(); reader.Close(); writer.Close(); workers.Wait() }()
	for _, child := range []*exec.Cmd{audio, capture, video} {
		log := &tailLog{}
		child.Stderr = log
		child.WaitDelay = 2 * time.Second
		if err := group.start(child); err != nil {
			return fmt.Errorf("start %s: %w\n%s", child.Path, err, log.String())
		}
		workers.Add(1)
		go func(cmd *exec.Cmd, log *tailLog) {
			defer workers.Done()
			err := cmd.Wait()
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("capture exited unexpectedly")
				}
				select {
				case fail <- fmt.Errorf("%s: %w\n%s", cmd.Path, err, log.String()):
				default:
				}
			}
		}(child, log)
	}
	// Parent must not retain pipe ends: helper exit must deliver EOF to encoder.
	reader.Close()
	writer.Close()
	ready()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-fail:
		return err
	}
}
