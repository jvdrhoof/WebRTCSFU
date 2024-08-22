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

var logger *Logger
var proxyConn *ProxyConnection
var clientID *int
var enableDebug bool

func main() {
	sfuAddress := flag.String("sfu", "localhost:8080", "SFU address")
	proxyPort := flag.String("p", ":0", "Port through which the DLL is connected")
	useProxyInput := flag.Bool("i", false, "Receive content from the DLL to forward over WebRTC")
	useProxyOutput := flag.Bool("o", false, "Forward content received over WebRTC to the DLL")
	clientID = flag.Int("c", 0, "Client ID")
	nTiles := flag.Int("t", 1, "Number of tiles")
	nQualities := flag.Int("q", 1, "Number of quality representations")
	enableDebugF := flag.Bool("d", false, "Enable debug instead of remote tiles")
	logLevel := flag.Int("l", LevelDefault, "Log level (0: default, 1: verbose, 2: debug)")
	flag.Parse()

	enableDebug = *enableDebugF

	logger = NewLogger(*logLevel)

	if *proxyPort == ":0" && !enableDebug {
		logger.Error("main", "Port cannot equal :0")
		os.Exit(1)
	}
	logger.Log("main", fmt.Sprintf("Starting client %d with %d tiles and %d qualities", *clientID, *nTiles, *nQualities), LevelDefault)

	var transcoder Transcoder
	if !enableDebug {
		proxyConn = NewProxyConnection()
		proxyConn.SetupConnection(*proxyPort)

		if *useProxyInput {
			proxyConn.StartListening(*nTiles, *nQualities)
			transcoder = NewTranscoderRemote(proxyConn)
		} else {
			transcoder = NewTranscoderDummy(proxyConn)
		}
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetSCTPMaxReceiveBufferSize(16 * 1024 * 1024)
	// TODO: When to use custom cipher suites?
	// settingEngine.SetDTLSCustomerCipherSuites()
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
	// TODO: Which enable support for multiple audio codecs + custom ones
	// TODO: also fix these settings although it probably doesnt matter that much
	audioCodecCapability := webrtc.RTPCodecCapability{
		MimeType:     "audio/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: nil,
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: codecCapability,
		PayloadType:        5,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: audioCodecCapability,
		PayloadType:        6,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}
	/*congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEMinBitrate(75_000*8), gcc.SendSideBWEInitialBitrate(75_000_000), gcc.SendSideBWEMaxBitrate(262_744_320))
	})
	if err != nil {
		panic(err)
	}*/

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	//m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)

	/*estimatorChan := make(chan cc.BandwidthEstimator, 1)
	congestionController.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
		estimatorChan <- estimator
	})

	i.Add(congestionController)*/
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, i); err != nil {
		panic(err)
	}

	//responder, _ := nack.NewResponderInterceptor()
	//i.Add(responder)

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeVideo)
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	//m.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeAudio)
	//if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeAudio); err != nil {
	//	panic(err)
	//}

	generator, err := twcc.NewSenderInterceptor(twcc.SendInterval(10 * time.Millisecond))
	if err != nil {
		panic(err)
	}
	i.Add(generator)

	//nackGenerator, _ := nack.NewGeneratorInterceptor()
	//i.Add(nackGenerator)

	var candidatesMux sync.Mutex
	pendingCandidates := make([]*webrtc.ICECandidate, 0)
	pendingCandidatesString := make([]string, 0)
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

	// TODO: probably remove this? This is the same as before but without feedback aka TWCC / NACK
	//
	codecCapability = webrtc.RTPCodecCapability{
		MimeType:     "video/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: nil,
	}

	// TODO: give audio custom id?
	audioTrack, err := NewTrackLocalAudioRTP(audioCodecCapability, fmt.Sprintf("audio_%d", *clientID), fmt.Sprintf("%d", 99))
	if err != nil {
		panic(err)
	}
	if _, err = peerConnection.AddTrack(audioTrack); err != nil {
		panic(err)
	}

	videoTracks := map[VideoKey]*TrackLocalCloudRTP{}
	for t := 0; t < *nTiles; t++ {
		for q := 0; q < *nQualities; q++ {
			videoTrack, err := NewTrackLocalCloudRTP(codecCapability, fmt.Sprintf("video_%d_%d_%d", *clientID, t, q), fmt.Sprintf("%d", q**nTiles+t), uint32(t), uint32(q))
			if err != nil {
				panic(err)
			}
			videoTracks[VideoKey{uint32(t), uint32(q)}] = videoTrack
			if _, err = peerConnection.AddTrack(videoTrack); err != nil {
				panic(err)
			}
		}
	}

	processRTCP := func(rtpSender *webrtc.RTPSender) {
		rtcpBuf := make([]byte, RTCPBufferLength)
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
			logger.Error("main", fmt.Sprintf("Cannot close peer connection: %v", cErr))
		}
	}()

	//estimator := <-estimatorChan

	// Create custom websocket handler on SFU address
	wsHandler := NewWSHandler(*sfuAddress, "/websocket", *nTiles, *nQualities)

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		logger.Log("OnICECandidate", fmt.Sprintf("New candidate: %s, %s", c.Address, c.String()), LevelVerbose)
		if !strings.HasPrefix(*sfuAddress, "193.190") || strings.HasPrefix(c.Address, "10.8.0") {
			logger.Log("OnICECandidate", fmt.Sprintf("Accepted candidate: %s, %s", c.Address, c.String()), LevelVerbose)
			candidatesMux.Lock()
			desc := peerConnection.RemoteDescription()
			if desc == nil {
				pendingCandidates = append(pendingCandidates, c)
			} else {
				payload := []byte(c.ToJSON().Candidate)
				wsHandler.SendMessage(WebsocketPacket{1, 4, string(payload)})
			}
			candidatesMux.Unlock()
		} else {
			logger.Log("OnICECandidate", fmt.Sprintf("Using WebRTC through the UGent Virtual Wall requires VPN-connected clients in the 10.8.0.0/24 address range, ignoring candidate: %s, %s", c.Address, c.String()), LevelVerbose)
		}
	})

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logger.Log("OnConnectionStateChange", fmt.Sprintf("Peer connection state has changed: %s", s.String()), LevelVerbose)

		if s == webrtc.PeerConnectionStateFailed {
			logger.Error("OnConnectionStateChange", "Peer connection failed, exiting")
			os.Exit(0)
		} else if s == webrtc.PeerConnectionStateConnected {
			// TODO Combine both write operations into one loop
			// Idk if the underlying socket is thread safe or not but having is an extra thread is probably unwarrented anyway

			logger.Log("OnConnectionStateChange", "Connected to the SFU", LevelDefault)

			go func() {
				// TODO: Potentially combine audio frames into single packet
				// Would add small delay but might be more optimal?
				// Atm audio frame is around 126 bytes so WebRTC overhead might be too much
				if enableDebug {
					cc := 0
					for {
						audioTrack.WriteAudioFrame([]byte("test"))
						if cc%100 == 0 {
							logger.Log("[SEND]", fmt.Sprintf("Audio frame %d", cc), LevelDebug)
						}
						time.Sleep(30 * time.Millisecond)
						cc++
					}
				} else {
					for {
						audioTrack.WriteAudioFrame(proxyConn.NextAudioFrame())
					}
				}

			}()

			// TODO probably remove this as it currently useless
			//targetBitrate := uint32(estimator.GetTargetBitrate())
			//transcoder.UpdateBitrate(100000000)

			for t := 0; t < *nTiles; t++ {
				for q := 0; q < *nQualities; q++ {
					// TODO maybe change writeframe into goroutines?
					go func(tileNr int, quality int) {
						cc := 0
						for {
							if err = videoTracks[VideoKey{uint32(tileNr), uint32(quality)}].WriteVideoFrame(transcoder, uint32(tileNr), uint32(quality)); err != nil {
								logger.Error("[SEND]", fmt.Sprintf("Failed to write incoming frame for tile %d and quality %d (ignoring for now)", tileNr, quality))
								continue
							}
							if enableDebug {
								if cc%100 == 0 {
									logger.Log("[SEND]", fmt.Sprintf("Video frame %d belonging to tile %d and quality %d", cc, tileNr, quality), LevelDebug)
								}
								time.Sleep(30 * time.Millisecond)
								cc++
							}
						}
					}(t, q)
				}
			}

			if enableDebug {
				// The following code is used to test the SFU's ability to change the quality of outgoing tracks
				go func() {
					decision := 0
					time.Sleep(4000 * time.Millisecond)
					otherClientStr := "1"
					if *clientID == 1 {
						otherClientStr = "2"
						for {
							if decision == 0 {
								wsHandler.SendMessage(WebsocketPacket{1, 7, otherClientStr + ",0"})
								decision = 1
							} else if decision == 1 {
								wsHandler.SendMessage(WebsocketPacket{1, 7, otherClientStr + ",1"})
								decision = 2
							} else if decision == 2 {
								wsHandler.SendMessage(WebsocketPacket{1, 7, otherClientStr + ",2"})
								decision = -1
							} else {
								wsHandler.SendMessage(WebsocketPacket{1, 7, otherClientStr + ",0"})
								decision = 0
							}
							time.Sleep(5000 * time.Millisecond)
						}
					}
				}()
			}
		}
	})

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		logger.Log("OnTrack", fmt.Sprintf("A new track with track ID %s is started", track.ID()), LevelDefault)
		logger.Log("OnTrack", fmt.Sprintf("MIME type %s, payload type %d, track SSRC %d, track ID %s, track StreamID %s", track.Codec().MimeType, track.PayloadType(), track.SSRC(), track.ID(), track.StreamID()), LevelVerbose)
		codecName := strings.Split(track.Codec().RTPCodecCapability.MimeType, "/")
		logger.Log("OnTrack", fmt.Sprintf("Track of type %d has started with codec %s", track.PayloadType(), codecName), LevelVerbose)

		// Create buffer to receive incoming track data, using 1300 bytes - header bytes
		// TODO is different for audio
		buf := make([]byte, BufferLength)
		// Allows to check if frames are received completely
		frames := make(map[uint32]uint32)
		// TODO: make clean seperated function for audio / video so we dont constantly need to do the kind check
		for {
			_, _, readErr := track.Read(buf)
			if readErr != nil {
				logger.Error("OnTrack", fmt.Sprintf("Can no longer read from track %s, terminating %s", track.ID(), readErr.Error()))
				break
			}
			if *useProxyOutput {
				if track.Kind() == webrtc.RTPCodecTypeVideo {
					proxyConn.SendTilePacket(buf, WebRTCHeaderSize)
				} else {
					proxyConn.SendAudioPacket(buf, WebRTCHeaderSize)
				}
			}
			// Are we dealing with video or audio?
			if track.Kind() == webrtc.RTPCodecTypeVideo {
				// Create a buffer from the byte array, skipping the first WebRTCHeaderSize bytes
				bufBinary := bytes.NewBuffer(buf[WebRTCHeaderSize:])
				// Read the fields from the buffer into a struct
				var p VideoFramePacket
				err := binary.Read(bufBinary, binary.LittleEndian, &p)
				if err != nil {
					panic(err)
				}

				// Uncomment to debug the actual content of packets
				/* if frames[p.FrameNr] == 0 {
					logger.Log("[RECV]", fmt.Sprintf("[Video] Bytes with total payload %v", buf[48:]), LevelDebug)
				} */

				frames[p.FrameNr] += p.SeqLen
				if frames[p.FrameNr] == p.FrameLen && p.FrameNr%100 == 0 {
					logger.Log("[RECV]", fmt.Sprintf("Received video frame %d from client %d and tile %d at quality %d with length %d", p.FrameNr, p.ClientNr, p.TileNr, p.Quality, p.FrameLen), LevelVerbose)
					// TODO: Check if this is correct (it shouldn't be)
					frames[p.FrameNr] = 0
				} else if frames[p.FrameNr] == p.FrameLen {
					logger.Log("[RECV]", fmt.Sprintf("Received video frame %d from client %d and tile %d at quality %d with length %d", p.FrameNr, p.ClientNr, p.TileNr, p.Quality, p.FrameLen), LevelDebug)
				}
			} else {
				// Create a buffer from the byte array, skipping the first WebRTCHeaderSize bytes
				bufBinary := bytes.NewBuffer(buf[WebRTCHeaderSize:])
				// Read the fields from the buffer into a struct
				var p AudioFramePacket
				err := binary.Read(bufBinary, binary.LittleEndian, &p)
				if err != nil {
					panic(err)
				}
				frames[p.FrameNr] += p.SeqLen
				if frames[p.FrameNr] == p.FrameLen && p.FrameNr%100 == 0 {
					logger.Log("[RECV]", fmt.Sprintf("Received audio frame %d from client %d with length %d", p.FrameNr, p.ClientNr, p.FrameLen), LevelVerbose)
				} else if frames[p.FrameNr] == p.FrameLen {
					logger.Log("[RECV]", fmt.Sprintf("Received audio frame %d from client %d with length %d", p.FrameNr, p.ClientNr, p.FrameLen), LevelDebug)
				}
			}
		}
	})

	var state = Idle
	logger.Log("main", fmt.Sprintf("Current state: %d", state), LevelVerbose)

	var handleMessageCallback = func(wsPacket WebsocketPacket) {
		switch wsPacket.MessageType {
		case 1: // hello
			logger.Log("handleMessageCallback", "Received a hello message", LevelVerbose)
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
			logger.Log("handleMessageCallback", fmt.Sprintf("Current state: %d", state), LevelVerbose)
		case 2: // offer
			logger.Log("handleMessageCallback", fmt.Sprintf("Received an offer: %s", wsPacket.Message), LevelVerbose)
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
			logger.Log("handleMessageCallback", fmt.Sprintf("Current state: %d", state), LevelVerbose)
		case 3: // answer
			logger.Log("handleMessageCallback", fmt.Sprintf("Received an answer: %s", wsPacket.Message), LevelVerbose)
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
			for _, c := range pendingCandidatesString {
				logger.Log("handleMessageCallback", fmt.Sprintf("Pending candidates string: %s", c), LevelVerbose)
				if candidateErr := peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: c}); candidateErr != nil {
					panic(candidateErr)
				}
			}
			candidatesMux.Unlock()
			state = Answer
			logger.Log("handleMessageCallback", fmt.Sprintf("Current state: %d", state), LevelVerbose)
		case 4: // candidate
			candidate := wsPacket.Message
			logger.Log("handleMessageCallback", fmt.Sprintf("New candidate received: %s", candidate), LevelVerbose)
			subnetworkID := 11 + 2**clientID
			if !strings.HasPrefix(*sfuAddress, "193.190") || strings.Contains(candidate, " 192.168."+strconv.Itoa(subnetworkID)) || strings.Contains(candidate, " 193.190") {
				logger.Log("handleMessageCallback", fmt.Sprintf("Considering candidate: %s", candidate), LevelVerbose)
				desc := peerConnection.RemoteDescription()
				if desc == nil {
					pendingCandidatesString = append(pendingCandidatesString, candidate)
				} else {
					if candidateErr := peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate}); candidateErr != nil {
						panic(candidateErr)
					}
				}
			}
		default:
			logger.Error("handleMessageCallback", fmt.Sprintf("Received non-compliant message of type %d", wsPacket.MessageType))
		}
	}

	wsHandler.StartListening(handleMessageCallback)

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

	tileNr  uint32
	quality uint32
}

// NewTrackLocalStaticSample returns a TrackLocalStaticSample
func NewTrackLocalCloudRTP(c webrtc.RTPCodecCapability, id, streamID string, tileNr uint32, quality uint32, options ...func(*webrtc.TrackLocalStaticRTP)) (*TrackLocalCloudRTP, error) {
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(c, id, streamID, options...)
	if err != nil {
		return nil, err
	}
	return &TrackLocalCloudRTP{
		rtpTrack: rtpTrack,
		tileNr:   tileNr,
		quality:  quality,
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

	s.packetizer = rtp.NewPacketizer(
		RTPPacketizerMTU, // Not MTU but ok
		0,                // Value is handled when writing
		0,                // Value is handled when writing
		NewPointCloudPayloader(s.tileNr, s.quality),
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

func (s *TrackLocalCloudRTP) WriteVideoFrame(t Transcoder, tile uint32, quality uint32) error {
	p := s.packetizer
	clockRate := s.clockRate
	if p == nil {
		return nil
	}
	samples := uint32(1 * clockRate)
	var data []byte
	if enableDebug {
		// Any data size can do, but a custom 5000 bytes was selected to use multiple packets
		data = make([]byte, 5000)
	} else {
		data = t.EncodeFrame(tile, quality)
	}

	if data != nil {
		packets := p.Packetize(data, samples)
		counter := 0

		for _, p := range packets {
			if err := s.rtpTrack.WriteRTP(p); err != nil {
				logger.Error("WriteFrame", fmt.Sprintf("%s", err))
			}
			counter += 1
		}
	}
	return nil
}

// TrackLocalStaticRTP  is a TrackLocal that has a pre-set codec and accepts RTP Packets.
// If you wish to send a media.Sample use TrackLocalStaticSample
type TrackLocalAudioRTP struct {
	packetizer rtp.Packetizer
	sequencer  rtp.Sequencer
	rtpTrack   *webrtc.TrackLocalStaticRTP
	clockRate  float64
}

// NewTrackLocalStaticSample returns a TrackLocalStaticSample
func NewTrackLocalAudioRTP(c webrtc.RTPCodecCapability, id, streamID string, options ...func(*webrtc.TrackLocalStaticRTP)) (*TrackLocalAudioRTP, error) {
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(c, id, streamID, options...)
	if err != nil {
		return nil, err
	}
	return &TrackLocalAudioRTP{
		rtpTrack: rtpTrack,
	}, nil
}

// Bind is called by the PeerConnection after negotiation is complete
// This asserts that the code requested is supported by the remote peer.
// If so it setups all the state (SSRC and PayloadType) to have a call
func (s *TrackLocalAudioRTP) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := s.rtpTrack.Bind(t)
	if err != nil {
		return codec, err
	}
	// We only need one packetizer
	if s.packetizer != nil {
		return codec, nil
	}
	s.sequencer = rtp.NewRandomSequencer()

	s.packetizer = rtp.NewPacketizer(
		RTPPacketizerMTU, // Not MTU but ok
		0,                // Value is handled when writing
		0,                // Value is handled when writing
		NewAudioPayloader(),
		s.sequencer,
		codec.ClockRate,
	)
	s.clockRate = float64(codec.RTPCodecCapability.ClockRate)
	return codec, err
}

// Unbind implements the teardown logic when the track is no longer needed. This happens
// because a track has been stopped.
func (s *TrackLocalAudioRTP) Unbind(t webrtc.TrackLocalContext) error {
	return s.rtpTrack.Unbind(t)
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (s *TrackLocalAudioRTP) ID() string { return s.rtpTrack.ID() }

// StreamID is the group this track belongs too. This must be unique
func (s *TrackLocalAudioRTP) StreamID() string { return s.rtpTrack.StreamID() }

// RID is the RTP stream identifier
func (s *TrackLocalAudioRTP) RID() string { return s.rtpTrack.RID() }

// Kind controls if this TrackLocal is audio or video
func (s *TrackLocalAudioRTP) Kind() webrtc.RTPCodecType { return s.rtpTrack.Kind() }

// Codec gets the Codec of the track
func (s *TrackLocalAudioRTP) Codec() webrtc.RTPCodecCapability {
	return s.rtpTrack.Codec()
}

func (s *TrackLocalAudioRTP) WriteAudioFrame(audio []byte) error {
	p := s.packetizer
	clockRate := s.clockRate
	if p == nil {
		return nil
	}
	samples := uint32(1 * clockRate)

	if audio != nil {
		packets := p.Packetize(audio, samples)
		counter := 0
		for _, p := range packets {
			if err := s.rtpTrack.WriteRTP(p); err != nil {
				logger.Error("WriteAudioFrame", fmt.Sprintf("%s", err))
			}
			counter += 1
		}
	}
	return nil
}
