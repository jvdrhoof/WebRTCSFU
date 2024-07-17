package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

var (
	addr     = flag.String("addr", ":8080", "http service address")
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	indexTemplate = &template.Template{}

	// lock for peerConnections, trackLocals and trackQualities
	listLock             sync.RWMutex
	qualitiesLock        sync.RWMutex
	peerConnections      []peerConnectionState
	trackLocals          map[string]*webrtc.TrackLocalStaticRTP
	trackQualities       map[string]map[int]*webrtc.TrackLocalStaticRTP
	settingEngine        webrtc.SettingEngine
	wsLock               sync.RWMutex
	maxNumberOfTiles     *int
	maxNumberOfQualities *int
	pcID                 = 0
)

type WebsocketPacket struct {
	ClientID    uint64
	MessageType uint64
	Message     string
}

type peerConnectionState struct {
	peerConnection          *webrtc.PeerConnection
	websocket               *threadSafeWriter
	ID                      int
	trackRTPSenders         map[string]*webrtc.RTPSender
	qualityDecisions        map[string]int
	pendingCandidatesString []string
}

var logger *Logger

func main() {
	maxNumberOfTiles = flag.Int("t", 1, "Number of tiles")
	maxNumberOfQualities = flag.Int("q", 1, "Number of qualities")
	logLevel := flag.Int("l", LevelDefault, "Log level (0: default, 1: verbose, 2: debug)")
	flag.Parse()

	logger = NewLogger(*logLevel)

	logger.Log("main", fmt.Sprintf("Starting SFU with %d tiles per client and %d qualities per tile", *maxNumberOfTiles, *maxNumberOfQualities), LevelDefault)

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetSCTPMaxReceiveBufferSize(16 * 1024 * 1024)

	// Init other state
	log.SetFlags(0)
	trackLocals = map[string]*webrtc.TrackLocalStaticRTP{}
	trackQualities = map[string]map[int]*webrtc.TrackLocalStaticRTP{}

	// Read index.html from disk into memory, serve whenever anyone requests /
	indexHTML, err := ioutil.ReadFile("index.html")
	if err != nil {
		indexHTML = []byte("<p>WebRTCSFU, Nothing to see here, please pass along</p>")
	}
	indexTemplate = template.Must(template.New("").Parse(string(indexHTML)))

	// WebSocket handler
	http.HandleFunc("/websocket", websocketHandler)

	// index.html handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			log.Fatal(err)
		}
	})

	// start HTTP server
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// Add a new sender track for either a video tile (initially at quality 0) or audio
func addTrack(pcState *peerConnectionState, t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()

	defer func() {
		listLock.Unlock()
		logger.LogClient(pcState.ID, "addTrack", "Calling signalPeerConnections to inform other clients", LevelVerbose)
		signalPeerConnections()
	}()
	logger.LogClient(pcState.ID, "addTrack", fmt.Sprintf("Adding track ID %s", t.ID()), LevelVerbose)
	trackLocals[t.ID()] = t
}

// Remove an existing sender track for video or audio
func removeTrack(pcState *peerConnectionState, t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()

	defer func() {
		listLock.Unlock()
		logger.LogClient(pcState.ID, "removeTrack", "Calling signalPeerConnections to inform other clients", LevelVerbose)
		signalPeerConnections()
	}()

	logger.LogClient(pcState.ID, "removeTrack", fmt.Sprintf("Removing track ID %s", t.ID()), LevelVerbose)
	delete(trackLocals, t.ID())
}

func addQualityTrack(pcState *peerConnectionState, t *webrtc.TrackLocalStaticRTP, quality int, newID string) {
	qualitiesLock.Lock()

	defer func() {
		qualitiesLock.Unlock()
	}()

	logger.LogClient(pcState.ID, "addQualityTrack", fmt.Sprintf("Adding incoming track of quality %d for track ID %s", quality, t.ID()), LevelVerbose)

	if _, exists := trackQualities[newID]; !exists {
		trackQualities[newID] = make(map[int]*webrtc.TrackLocalStaticRTP)
	}
	trackQualities[newID][quality] = t
}

// Add a new listening track for either a video tile (initially at quality 0) or audio
func addNewTrackForPeer(pcState *peerConnectionState, trackID string) {
	v := strings.Split(string(trackID), "_")
	if v[0] == "audio" {
		logger.LogClient(pcState.ID, "addNewTrackForPeer", fmt.Sprintf("Adding a new listening track for audio with track ID %s", trackID), LevelDebug)
		trackLocal := trackLocals[trackID]
		if sender, err := pcState.peerConnection.AddTrack(trackLocal); err != nil {
			panic(err)
		} else {
			pcState.trackRTPSenders[trackID] = sender
		}
	} else {
		qualitiesLock.Lock()

		defer func() {
			qualitiesLock.Unlock()
		}()

		logger.LogClient(pcState.ID, "addNewTrackForPeer", fmt.Sprintf("Adding a new listening track for video with track ID %s", trackID), LevelDebug)
		trackLocal := trackQualities[trackID][0]
		if sender, err := pcState.peerConnection.AddTrack(trackLocal); err != nil {
			panic(err)
		} else {
			pcState.trackRTPSenders[trackID] = sender
			pcState.qualityDecisions[trackID] = 0
		}
	}
}

// Change the quality representation at which to retrieve a video tile
func setTrackQuality(pcState *peerConnectionState, trackID string, quality int) {
	qualitiesLock.Lock()
	listLock.Lock()

	defer func() {
		qualitiesLock.Unlock()
		listLock.Unlock()
	}()

	if oldQuality, keyExists := pcState.qualityDecisions[trackID]; keyExists {
		if oldQuality != quality {
			logger.LogClient(pcState.ID, "setTrackQuality", fmt.Sprintf("Trying to adopt the decision for track with ID %s to use quality %d instead of quality %d", trackID, quality, oldQuality), LevelVerbose)

			if rtpSender, keyExists := pcState.trackRTPSenders[trackID]; keyExists {
				if quality < 0 {
					logger.LogClient(pcState.ID, "setTrackQuality", "Replacing track with NIL", LevelVerbose)
					rtpSender.ReplaceTrack(nil)
				} else {
					if trackQuality, keyExists := trackQualities[trackID][quality]; keyExists {
						logger.LogClient(pcState.ID, "setTrackQuality", fmt.Sprintf("Replacing current quality track with ID %s with updated quality %d", trackQuality.ID(), quality), LevelVerbose)
						rtpSender.ReplaceTrack(trackQuality)
					} else {
						logger.LogClient(pcState.ID, "setTrackQuality", fmt.Sprintf("Quality %d not found in map trackQualities[trackID], leaving track set to quality %d", quality, oldQuality), LevelVerbose)
						quality = oldQuality
					}
				}
			}
			pcState.qualityDecisions[trackID] = quality
		}
	} else {
		logger.LogClient(pcState.ID, "setTrackQuality", fmt.Sprintf("Track ID %s not found in map qualityDecisions, leaving quality unchanged", trackID), LevelVerbose)
	}
}

// Updates each PeerConnection so that it is getting all the expected media tracks
func signalPeerConnections() {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
	}()

	attemptSync := func() (tryAgain bool) {
		for i := range peerConnections {
			if peerConnections[i].peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
				peerConnections = append(peerConnections[:i], peerConnections[i+1:]...)
				return true // We modified the slice, start from the beginning
			}

			// map of sender we already are sending, so we don't double send
			existingSenders := map[string]bool{}

			for _, sender := range peerConnections[i].peerConnection.GetSenders() {
				if sender.Track() == nil {
					continue
				}
				existingSenders[sender.Track().ID()] = true

				// If we have an RTPSender that doesn't map to an existing track remove and signal
				if _, ok := trackLocals[sender.Track().ID()]; !ok {
					delete(peerConnections[i].trackRTPSenders, sender.Track().ID())
					if err := peerConnections[i].peerConnection.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Don't receive videos we are sending, make sure we don't have loopback
			for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}
				trackID := receiver.Track().ID()
				v := strings.Split(trackID, "_")
				if v[0] == "video" {
					clientID, err := strconv.Atoi(v[1])
					if err != nil {
						logger.ErrorClient(peerConnections[i].ID, "signalPeerConnections", fmt.Sprintf("Client ID %s cannot be converted to an integer", v[1]))
						panic(err)
					}
					tileID, err := strconv.Atoi(v[2])
					if err != nil {
						logger.ErrorClient(peerConnections[i].ID, "signalPeerConnections", fmt.Sprintf("Tile ID %s cannot be converted to an integer", v[2]))
						panic(err)
					}
					trackID = fmt.Sprintf("video_%d_%d", clientID, tileID)
				}
				existingSenders[trackID] = true
			}

			// Add all tracks we aren't sending yet to the PeerConnection
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					addNewTrackForPeer(&peerConnections[i], trackID)
				}
			}

			offer, err := peerConnections[i].peerConnection.CreateOffer(nil)
			if err != nil {
				return true
			}

			if err = peerConnections[i].peerConnection.SetLocalDescription(offer); err != nil {
				return true
			}

			payload, err := json.Marshal(offer)
			if err != nil {
				return true
			}

			logger.LogClient(peerConnections[i].ID, "signalPeerConnections", "Sending offer to peer connection", LevelVerbose)
			s := fmt.Sprintf("%d@%d@%s", 0, 2, string(payload))
			wsLock.Lock()
			peerConnections[i].websocket.WriteMessage(websocket.TextMessage, []byte(s))
			wsLock.Unlock()
		}

		return
	}

	logger.Log("signalPeerConnections", "Attemping to synchronize the available tracks", LevelVerbose)

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 1 {
			// Release the lock and attempt a sync in 5 seconds
			// We might be blocking a RemoveTrack or AddTrack
			go func() {
				time.Sleep(time.Second * 5)
				signalPeerConnections()
			}()
			return
		}

		if !attemptSync() {
			break
		}
	}
}

// Handle incoming websockets
func websocketHandler(w http.ResponseWriter, r *http.Request) {
	logger.Log("websocketHandler", fmt.Sprintf("Web socket handler targeted with URL query %s", r.URL.Query()), LevelVerbose)
	numberOfTiles := *maxNumberOfTiles
	numberOfTilesS := r.URL.Query().Get("ntiles")
	if numberOfTilesS != "" {
		i, err := strconv.Atoi(numberOfTilesS)
		if err == nil {
			numberOfTiles = i
		}
	}
	numberOfQualities := 1
	numberOfQualitiesS := r.URL.Query().Get("nqualities")
	if numberOfQualitiesS != "" {
		i, err := strconv.Atoi(numberOfQualitiesS)
		if err == nil {
			numberOfQualities = i
		}
	}
	listLock.Lock()
	currentPCID := pcID
	pcID++
	listLock.Unlock()

	logger.Log("websocketHandler", fmt.Sprintf("New connection received for address %s, assigning client ID #%d", r.RemoteAddr, currentPCID), LevelVerbose)
	logger.LogClient(currentPCID, "websocketHandler", "Websocket handler started", LevelDebug)

	// Upgrade HTTP request to Websocket
	unsafeWebSocketConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.ErrorClient(currentPCID, "websocketHandler", fmt.Sprintf("%s", err))
		return
	}

	logger.LogClient(currentPCID, "websocketHandler", "Websocket handler upgraded", LevelDebug)

	webSocketConnection := &threadSafeWriter{unsafeWebSocketConn, sync.Mutex{}}
	// When this frame returns close the Websocket
	defer func() {
		logger.LogClient(currentPCID, "websocketHandler", "Closing a ThreadSafeWriter", LevelDebug)
		webSocketConnection.Close()
	}()

	logger.LogClient(currentPCID, "websocketHandler", "Creating a new peer connection", LevelVerbose)

	mediaEngine := &webrtc.MediaEngine{}
	interceptorRegistry := &interceptor.Registry{}

	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	videoRTCPFeedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb", Parameter: ""},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack", Parameter: ""},
		{Type: "nack", Parameter: "pli"},
	}

	videoCodecCapability := webrtc.RTPCodecCapability{
		MimeType:     "video/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: videoRTCPFeedback,
	}

	// TODO: audio RTP
	audioCodecCapability := webrtc.RTPCodecCapability{
		MimeType:     "audio/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: nil,
	}

	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: videoCodecCapability,
		PayloadType:        5,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: audioCodecCapability,
		PayloadType:        6,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	mediaEngine.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	//mediaEngine.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	mediaEngine.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeVideo)
	if err := mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	trackMerger, _ := NewTrackMergerInterceptor()
	interceptorRegistry.Add(trackMerger)

	/*congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(1_000_000))
	})
	if err != nil {
		panic(err)
	}

	estimatorChan := make(chan cc.BandwidthEstimator, 1)
	congestionController.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
		estimatorChan <- estimator
	})

	interceptorRegistry.Add(congestionController)*/

	if err = webrtc.ConfigureTWCCHeaderExtensionSender(mediaEngine, interceptorRegistry); err != nil {
		panic(err)
	}
	if err = webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		panic(err)
	}

	peerConnection, err := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptorRegistry)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	logger.LogClient(currentPCID, "websocketHandler", "Peer connection created", LevelVerbose)

	// When this frame returns close the PeerConnection
	defer func() {
		logger.LogClient(currentPCID, "websocketHandler", "Closing a peer connection", LevelVerbose)
		peerConnection.Close()
	}()

	logger.LogClient(currentPCID, "websocketHandler", "Adding audio track", LevelVerbose)
	if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		logger.ErrorClient(currentPCID, "websocketHandler", fmt.Sprintf("When adding audio transceiver: %s", err))
		return
	}

	logger.LogClient(currentPCID, "websocketHandler", fmt.Sprintf("Iterating and adding %d video tracks", numberOfTiles*numberOfQualities), LevelVerbose)
	for i := 0; i < numberOfTiles*numberOfQualities; i++ {
		if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			logger.ErrorClient(currentPCID, "websocketHandler", fmt.Sprintf("When adding video transceiver: %s", err))
			return
		}
	}

	logger.LogClient(currentPCID, "websocketHandler", "Waiting for lock to add connection to connection list", LevelVerbose)

	// Add our new PeerConnection to global list
	listLock.Lock()
	var pcState = peerConnectionState{peerConnection, webSocketConnection, currentPCID, make(map[string]*webrtc.RTPSender), make(map[string]int), make([]string, 0)}
	//pcID += 1
	peerConnections = append(peerConnections, pcState)
	listLock.Unlock()

	// Trickle ICE and emit server candidate to client
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		logger.LogClient(pcState.ID, "OnICECandidate", fmt.Sprintf("New candidate with address %s and port %d", i.Address, i.Port), LevelVerbose)
		payload := []byte(i.ToJSON().Candidate)
		s := fmt.Sprintf("%d@%d@%s", 0, 4, string(payload))
		wsLock.Lock()
		err = webSocketConnection.WriteMessage(websocket.TextMessage, []byte(s))
		wsLock.Unlock()
		if err != nil {
			panic(err)
		}
	})

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		logger.LogClient(pcState.ID, "OnConnectionStateChange", fmt.Sprintf("Peer connection state has changed to %s", p.String()), LevelVerbose)
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				logger.ErrorClient(pcState.ID, "OnConnectionStateChange", fmt.Sprintf("%s", err))
			}
		case webrtc.PeerConnectionStateClosed:
			logger.LogClient(pcState.ID, "OnConnectionStateChange", "Peer connection closed", LevelVerbose)
			signalPeerConnections()
		case webrtc.PeerConnectionStateConnected:
			logger.LogClient(pcState.ID, "OnConnectionStateChange", "Peer connected", LevelVerbose)
		}
	})

	// Called when the packet is sent over this track
	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		logger.LogClient(pcState.ID, "OnTrack", fmt.Sprintf("ID %s, StreamID %s", t.ID(), t.StreamID()), LevelVerbose)
		var trackLocal *webrtc.TrackLocalStaticRTP
		var err error
		v := strings.Split(string(t.ID()), "_")
		if v[0] == "audio" {
			trackLocal, err = webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
			if err != nil {
				logger.ErrorClient(pcState.ID, "OnTrack", fmt.Sprintf("Failed to create a track with ID %s and StreamID %s", t.ID(), t.StreamID()))
				panic(err)
			}
			addTrack(&pcState, trackLocal)
			defer func() {
				removeTrack(&pcState, trackLocal)
			}()
		} else {
			clientID, err := strconv.Atoi(v[1])
			if err != nil {
				logger.ErrorClient(pcState.ID, "OnTrack", fmt.Sprintf("Client ID %s cannot be converted to an integer", v[1]))
				panic(err)
			}
			tileID, err := strconv.Atoi(v[2])
			if err != nil {
				logger.ErrorClient(pcState.ID, "OnTrack", fmt.Sprintf("Tile ID %s cannot be converted to an integer", v[2]))
				panic(err)
			}
			quality, err := strconv.Atoi(v[3])
			if err != nil {
				logger.ErrorClient(pcState.ID, "OnTrack", fmt.Sprintf("Quality %s cannot be converted to an integer", v[3]))
				panic(err)
			}
			newID := fmt.Sprintf("video_%d_%d", clientID, tileID)
			newStreamID := fmt.Sprintf("%d", tileID)
			logger.LogClient(pcState.ID, "OnTrack", fmt.Sprintf("New track with ID %s and StreamID %s", newID, newStreamID), LevelVerbose)
			trackLocal, err = webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, newID, newStreamID)
			if err != nil {
				logger.ErrorClient(pcState.ID, "OnTrack", fmt.Sprintf("Failed to create a track with ID %s and StreamID %s", newID, newStreamID))
				panic(err)
			}
			addQualityTrack(&pcState, trackLocal, quality, newID)
			if quality == 0 {
				addTrack(&pcState, trackLocal)
				defer func() {
					removeTrack(&pcState, trackLocal)
				}()
			}
		}
		buf := make([]byte, 1500)
		for {

			i, _, err := t.Read(buf)
			if err != nil {
				logger.ErrorClient(pcState.ID, "OnTrack", fmt.Sprintf("Track %s during read operation: %s", trackLocal.ID(), err))
				break
			}
			if _, err = trackLocal.Write(buf[:i]); err != nil {
				logger.ErrorClient(pcState.ID, "OnTrack", fmt.Sprintf("Track %s during write operation: %s", trackLocal.ID(), err))
				break
			}
		}
	})

	logger.LogClient(pcState.ID, "websocketHandler", "Will now call signalpeerconnections again", LevelVerbose)
	signalPeerConnections()

	for {
		_, raw, err := webSocketConnection.ReadMessage()
		if err != nil {
			logger.ErrorClient(pcState.ID, "websocketHandler", fmt.Sprintf("ReadMessage: %s", err))
			break
		}
		v := strings.Split(string(raw), "@")
		messageType, _ := strconv.ParseUint(v[1], 10, 64)
		message := v[2]
		logger.LogClient(pcState.ID, "websocketHandler", fmt.Sprintf("Message type: %d (%s)", messageType, websocketMessageTypeToString(messageType)), LevelVerbose)
		switch messageType {
		case 3: // answer
			answer := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message), &answer); err != nil {
				panic(err)
			}
			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				panic(err)
			}
			for _, c := range pcState.pendingCandidatesString {
				if candidateErr := peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: c}); candidateErr != nil {
					panic(candidateErr)
				}
			}
		case 4: // candidate
			desc := peerConnection.RemoteDescription()
			if desc == nil {
				pcState.pendingCandidatesString = append(pcState.pendingCandidatesString, message)
			} else {
				if candidateErr := peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: message}); candidateErr != nil {
					panic(candidateErr)
				}
			}
		case 7: // quality decisions
			logger.LogClient(pcState.ID, "websocketHandler", fmt.Sprintf("Implementing quality decisions: %s", message), LevelVerbose)
			v := strings.Split(message, ";")
			for _, w := range v {
				x := strings.Split(w, ",")
				clientID := x[0]
				for tileID, sQuality := range x[1:] {
					trackID := fmt.Sprintf("video_%s_%d", clientID, tileID)
					quality, err := strconv.Atoi(sQuality)
					if err != nil {
						logger.ErrorClient(pcState.ID, "websocketHandler", fmt.Sprintf("The string %s should contain an integer representing the preferred quality level, but cannot be converted to an integer", sQuality))
						continue
					}
					setTrackQuality(&pcState, trackID, quality)
				}
			}
		}
	}
}

func websocketMessageTypeToString(messageType uint64) string {
	switch messageType {
	case 3: // answer
		return "Answer with Remote Description"
	case 4: // candidate
		return "New ICE Candidate"
	case 7: // quality decisions
		return "Quality decisions"
	}
	return "Unknown Message Type"
}

// Helper to make Gorilla Websockets threadsafe
type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}
