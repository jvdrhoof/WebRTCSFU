package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

const (
	Idle     int = 0
	Hello    int = 1
	Offer    int = 2
	Answer   int = 3
	Ready    int = 4
	Finished int = 5
)

var proxyConn *ProxyConnection

func main() {
	offerAddr := flag.String("offer-address", ":7002", "Address that the Offer HTTP server is hosted on.")
	answerAddr := flag.String("r", "127.0.0.1:8080", "Address that the Answer HTTP server is hosted on.")
	useFiles := flag.Bool("f", false, "Use pre encoded files instead or remote input")
	useVirtualWall := flag.Bool("v", false, "Use virtual wall ip filter")
	proxyPort := flag.String("p", ":0", "Use as a proxy with specified port")
	useProxyInput := flag.Bool("i", false, "Use proxy as input for the frames")
	useProxyOutput := flag.Bool("o", false, "Send the frames to the proxy")
	contentDirectory := flag.String("d", "content_jpg", "Content directory")
	doNotSendData := flag.Bool("n", false, "Do not send data")
	flag.Parse()
	useProxy := false
	numberOfTiles := 1

	if *proxyPort != ":0" {
		proxyConn = NewProxyConnection()
		fmt.Println(*proxyPort)

		var handleSetupCallback = func(NumberOfTiles int) {
			fmt.Println("Number of tiles equals ", NumberOfTiles)
			numberOfTiles = NumberOfTiles
		}

		proxyConn.SetupConnection(*proxyPort, handleSetupCallback)
		useProxy = true
		// TODO maybe move this
		if *useProxyInput {
			proxyConn.StartListening()
		}
	}

	println(*offerAddr, *answerAddr, *useFiles, *useVirtualWall, *useProxyInput, *useProxyOutput, *contentDirectory)
	var transcoder Transcoder
	if !*useProxyInput && !*doNotSendData {
		transcoder = NewTranscoderFile(*contentDirectory)
	} else if *useProxyInput {
		transcoder = NewTranscoderRemote(proxyConn)
	} else {
		transcoder = NewTranscoderDummy(proxyConn)
	}

	for !transcoder.IsReady() {
		time.Sleep(10 * time.Millisecond)
	}

	/*// Send HTTP request to SFU
	resp, err := http.Get(fmt.Sprintf("http://%s/", *answerAddr))
	if err != nil {
		panic(err)
	} else if err := resp.Body.Close(); err != nil {
		panic(err)
	}*/

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetSCTPMaxReceiveBufferSize(16 * 1024 * 1024)

	i := &interceptor.Registry{}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}
	videoRTCPFeedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb", Parameter: ""},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack", Parameter: ""},
		{Type: "nack", Parameter: "pli"},
	}

	codecCapability := webrtc.RTPCodecCapability{
		MimeType:     "video/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: videoRTCPFeedback,
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: codecCapability,
		PayloadType:        5,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// Sender side

	congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEMinBitrate(75_000*8), gcc.SendSideBWEInitialBitrate(75_000_000), gcc.SendSideBWEMaxBitrate(262_744_320))
	})
	if err != nil {
		panic(err)
	}

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)

	estimatorChan := make(chan cc.BandwidthEstimator, 1)
	congestionController.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
		estimatorChan <- estimator
	})

	i.Add(congestionController)
	if err = webrtc.ConfigureTWCCHeaderExtensionSender(m, i); err != nil {
		panic(err)
	}

	responder, _ := nack.NewResponderInterceptor()
	i.Add(responder)

	// Client side

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeVideo)
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeAudio)
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	generator, err := twcc.NewSenderInterceptor(twcc.SendInterval(10 * time.Millisecond))
	if err != nil {
		panic(err)
	}
	i.Add(generator)

	nackGenerator, _ := nack.NewGeneratorInterceptor()
	i.Add(nackGenerator)

	var candidatesMux sync.Mutex
	pendingCandidates := make([]*webrtc.ICECandidate, 0)
	peerConnection, err := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(i), webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		panic(err)
	}

	codecCapability = webrtc.RTPCodecCapability{
		MimeType:     "video/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: nil,
	}

	videoTracks := map[int]*TrackLocalCloudRTP{}

	for i := 0; i < numberOfTiles; i++ {
		videoTrack, err := NewTrackLocalCloudRTP(codecCapability, fmt.Sprintf("video_%d_%d", proxyPort, i), fmt.Sprintf("%d", i))
		if err != nil {
			panic(err)
		}
		videoTracks[i] = videoTrack
	}

	for i := 0; i < numberOfTiles; i++ {
		if _, err = peerConnection.AddTrack(videoTracks[i]); err != nil {
			panic(err)
		}
	}

	processRTCP := func(rtpSender *webrtc.RTPSender) {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}

	for _, rtpSender := range peerConnection.GetSenders() {
		go processRTCP(rtpSender)
	}

	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("Cannot close peer connection: %v\n", cErr)
		}
	}()

	estimator := <-estimatorChan

	// Create custom websocket handler on correct address (local for now)
	wsHandler := NewWSHandler("127.0.0.1:8080", "/websocket")

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidatesMux.Lock()
		desc := peerConnection.RemoteDescription()
		if desc == nil {
			pendingCandidates = append(pendingCandidates, c)
		} else {
			payload := []byte(c.ToJSON().Candidate)
			wsHandler.SendMessage(WebsocketPacket{1, 4, string(payload)})
		}
		candidatesMux.Unlock()
	})

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer connection state has changed: %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer connection has gone to failed exiting")
			os.Exit(0)
		} else if s == webrtc.PeerConnectionStateConnected {
			for ; true; <-time.NewTicker(20 * time.Millisecond).C {
				targetBitrate := uint32(estimator.GetTargetBitrate())
				transcoder.UpdateBitrate(targetBitrate)

				for i := 0; i < numberOfTiles; i++ {
					if err = videoTracks[i].WriteFrame(transcoder, uint32(i)); err != nil {
						panic(err)
					}
				}
			}
		}
	})

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		println("OnTrack has been called")
		println("MIME type:", track.Codec().MimeType)
		println("Payload type:", track.PayloadType())

		if track.ID()[len(track.ID())-1] == '1' {
			wsHandler.SendMessage(WebsocketPacket{1, 5, track.ID()})
		}

		codecName := strings.Split(track.Codec().RTPCodecCapability.MimeType, "/")
		fmt.Printf("Track of type %d has started: %s\n", track.PayloadType(), codecName)

		// Create buffer to receive incoming track data, using 1300 bytes - header bytes
		buf := make([]byte, 1220)

		// Allows to check if frames are received completely
		// Frame number and corresponding length
		frames := make(map[uint32]uint32)
		for {
			_, _, readErr := track.Read(buf)
			if readErr != nil {
				panic(err)
			}

			/*
				var y RemoteInputPacketHeader
				e := binary.Read(bytes.NewBuffer((buf[20:40])), binary.LittleEndian, &y)
				if e != nil {
					fmt.Println("Error:", e)
					fmt.Println("QUITING")
					return
				}
				fmt.Println("Packet ", y.Framenr, y.Tilenr, y.Tilelen, y.Frameoffset, y.Packetlen)
			*/

			if useProxy && *useProxyOutput {
				// TODO: Use bufBinary and make plugin buffer size as parameter
				// proxyConn.SendFramePacket(buf, 20)
				proxyConn.SendTilePacket(buf, 20)
			}
			// Create a buffer from the byte array, skipping the first 20 WebRTC bytes
			// TODO: mention WebRTC header content explicitly
			bufBinary := bytes.NewBuffer(buf[20:])
			// Read the fields from the buffer into a struct
			var p FramePacket
			err := binary.Read(bufBinary, binary.LittleEndian, &p)
			if err != nil {
				panic(err)
			}
			frames[p.FrameNr] += p.SeqLen
			if frames[p.FrameNr] == p.TileLen && p.FrameNr%100 == 0 {
				println("FRAME COMPLETE ", p.FrameNr, p.TileLen)
			}
		}
	})

	var state = Idle
	println("Current state:", state)

	var handleMessageCallback = func(wsPacket WebsocketPacket) {
		switch wsPacket.MessageType {
		case 1: // hello
			println("Received hello")
			offer, err := peerConnection.CreateOffer(nil)
			if err != nil {
				panic(err)
			}
			if err = peerConnection.SetLocalDescription(offer); err != nil {
				panic(err)
			}
			payload, err := json.Marshal(offer)
			if err != nil {
				panic(err)
			}
			wsHandler.SendMessage(WebsocketPacket{1, 2, string(payload)})
			state = Hello
			println("Current state:", state)
		case 2: // offer
			println("Received offer")
			offer := webrtc.SessionDescription{}
			err := json.Unmarshal([]byte(wsPacket.Message), &offer)
			if err != nil {
				panic(err)
			}
			err = peerConnection.SetRemoteDescription(offer)
			if err != nil {
				panic(err)
			}
			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				panic(err)
			}
			if err = peerConnection.SetLocalDescription(answer); err != nil {
				panic(err)
			}
			payload, err := json.Marshal(answer)
			if err != nil {
				panic(err)
			}
			wsHandler.SendMessage(WebsocketPacket{1, 3, string(payload)})
			state = Offer
			println("Current state:", state)
		case 3: // answer
			println("Received answer")
			answer := webrtc.SessionDescription{}
			err := json.Unmarshal([]byte(wsPacket.Message), &answer)
			if err != nil {
				panic(err)
			}
			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				panic(err)
			}
			candidatesMux.Lock()
			for _, c := range pendingCandidates {
				payload := []byte(c.ToJSON().Candidate)
				wsHandler.SendMessage(WebsocketPacket{1, 4, string(payload)})
			}
			candidatesMux.Unlock()
			state = Answer
			println("Current state:", state)
		case 4: // candidate
			println("Received candidate")
			candidate := wsPacket.Message
			if candidateErr := peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate}); candidateErr != nil {
				panic(candidateErr)
			}
		default:
			println(fmt.Sprintf("Received non-compliant message type %d", wsPacket.MessageType))
		}
	}

	wsHandler.StartListening(handleMessageCallback)

	/*offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}
	if err = peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}
	payload, err := json.Marshal(offer)
	if err != nil {
		panic(err)
	}
	wsHandler.SendMessage(WebsocketPacket{1, 2, string(payload)})
	state = Hello
	println("Current state:", state)*/

	// Block forever
	select {}
}

// TrackLocalStaticRTP  is a TrackLocal that has a pre-set codec and accepts RTP Packets.
// If you wish to send a media.Sample use TrackLocalStaticSample
type TrackLocalCloudRTP struct {
	packetizer rtp.Packetizer
	sequencer  rtp.Sequencer
	rtpTrack   *webrtc.TrackLocalStaticRTP
	clockRate  float64
}

// NewTrackLocalStaticSample returns a TrackLocalStaticSample
func NewTrackLocalCloudRTP(c webrtc.RTPCodecCapability, id, streamID string, options ...func(*webrtc.TrackLocalStaticRTP)) (*TrackLocalCloudRTP, error) {
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(c, id, streamID, options...)
	if err != nil {
		return nil, err
	}
	return &TrackLocalCloudRTP{
		rtpTrack: rtpTrack,
	}, nil
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it setups all the state (SSRC and PayloadType) to have a call
func (s *TrackLocalCloudRTP) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := s.rtpTrack.Bind(t)
	if err != nil {
		return codec, err
	}

	// We only need one packetizer
	if s.packetizer != nil {
		return codec, nil
	}

	s.sequencer = rtp.NewRandomSequencer()
	ui64, err := strconv.ParseUint(s.StreamID(), 10, 64)
	if err != nil {
		panic(err)
	}
	s.packetizer = rtp.NewPacketizer(
		1200, // Not MTU but ok
		0,    // Value is handled when writing
		0,    // Value is handled when writing
		NewPointCloudPayloader(uint32(ui64)),
		s.sequencer,
		codec.ClockRate,
	)

	s.clockRate = float64(codec.RTPCodecCapability.ClockRate)
	return codec, err
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (s *TrackLocalCloudRTP) Unbind(t webrtc.TrackLocalContext) error {
	return s.rtpTrack.Unbind(t)
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (s *TrackLocalCloudRTP) ID() string { return s.rtpTrack.ID() }

// StreamID is the group this track belongs too. This must be unique
func (s *TrackLocalCloudRTP) StreamID() string { return s.rtpTrack.StreamID() }

// RID is the RTP stream identifier
func (s *TrackLocalCloudRTP) RID() string { return s.rtpTrack.RID() }

// Kind controls if this TrackLocal is audio or video
func (s *TrackLocalCloudRTP) Kind() webrtc.RTPCodecType { return s.rtpTrack.Kind() }

// Codec gets the Codec of the track
func (s *TrackLocalCloudRTP) Codec() webrtc.RTPCodecCapability {
	return s.rtpTrack.Codec()
}

func (s *TrackLocalCloudRTP) WriteFrame(t Transcoder, tile uint32) error {
	p := s.packetizer
	clockRate := s.clockRate

	if p == nil {
		return nil
	}

	samples := uint32(1 * clockRate)
	frameData := t.EncodeFrame(tile)
	if frameData != nil {
		packets := p.Packetize(frameData.Data, samples)
		writeErrs := []error{}
		counter := 0
		for _, p := range packets {
			if err := s.rtpTrack.WriteRTP(p); err != nil {
				writeErrs = append(writeErrs, err)
			}
			counter += 1
		}
		if len(writeErrs) > 0 {
			println(writeErrs)
		}
	}
	return nil
}

// AV1Payloader payloads AV1 packets
type PointCloudPayloader struct {
	frameCounter uint32
	tile         uint32
}

// Payload fragments a AV1 packet across one or more byte arrays
// See AV1Packet for description of AV1 Payload Header
func (p *PointCloudPayloader) Payload(mtu uint16, payload []byte) (payloads [][]byte) {
	payloadDataOffset := uint32(0)
	payloadLen := uint32(len(payload))
	payloadRemaining := payloadLen
	for payloadRemaining > 0 {
		currentFragmentSize := uint32(1148)
		if payloadRemaining < currentFragmentSize {
			currentFragmentSize = payloadRemaining
		}
		p := NewFramePacket(p.frameCounter, p.tile, payloadLen, payloadDataOffset, currentFragmentSize, payload)
		buf := new(bytes.Buffer)

		if err := binary.Write(buf, binary.LittleEndian, p); err != nil {
			panic(err)
		}
		payloads = append(payloads, buf.Bytes())
		payloadDataOffset += currentFragmentSize
		payloadRemaining -= currentFragmentSize
	}
	p.frameCounter++
	/*
		for index, element := range payloads {
			var y RemoteInputPacketHeader
			err := binary.Read(bytes.NewBuffer((element[:20])), binary.LittleEndian, &y)
			if err != nil {
				fmt.Println("Error:", err)
				fmt.Println("QUITING")
				return
			}
			fmt.Println("Packet ", index, y.Framenr, y.Tilenr, y.Packetlen, y.Frameoffset, y.Tilelen)
		}
	*/
	return payloads
}

func NewPointCloudPayloader(tile uint32) *PointCloudPayloader {
	return &PointCloudPayloader{0, tile}
}
