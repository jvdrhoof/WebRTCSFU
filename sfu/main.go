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
	listLock         sync.RWMutex
	qualitiesLock    sync.RWMutex
	peerConnections  []peerConnectionState
	trackLocals      map[string]*webrtc.TrackLocalStaticRTP
	trackQualities   map[string]map[int]*webrtc.TrackLocalStaticRTP
	settingEngine    webrtc.SettingEngine
	wsLock           sync.RWMutex
	maxNumberOfTiles *int
	pcID             = 0
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

func main() {
	maxNumberOfTiles = flag.Int("t", 1, "Number of tiles")
	flag.Parse()

	fmt.Printf("WebRTCSFU: Starting SFU with default %d tiles per client\n", *maxNumberOfTiles)

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
		fmt.Printf("WebRTCSFU: [Client #%d] addTrack: Calling signalPeerConnections to inform other clients\n", pcState.ID)
		signalPeerConnections()
	}()

	fmt.Printf("WebRTCSFU: [Client #%d] addTrack: t.ID %s\n", pcState.ID, t.ID())
	trackLocals[t.ID()] = t
}

// Remove an existing sender track for video or audio
func removeTrack(pcState *peerConnectionState, t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()

	defer func() {
		listLock.Unlock()
		fmt.Printf("WebRTCSFU: [Client #%d] removeTrack: Calling signalPeerConnections to inform other clients\n", pcState.ID)
		signalPeerConnections()
	}()

	fmt.Printf("WebRTCSFU: [Client #%d] removeTrack: t.ID %s\n", pcState.ID, t.ID())
	delete(trackLocals, t.ID())
}

func addQualityTrack(pcState *peerConnectionState, t *webrtc.TrackLocalStaticRTP, quality int, newID string) {
	qualitiesLock.Lock()

	defer func() {
		qualitiesLock.Unlock()
	}()

	if _, exists := trackQualities[newID]; !exists {
		trackQualities[newID] = make(map[int]*webrtc.TrackLocalStaticRTP)
		trackQualities[newID][-1] = nil
	}
	trackQualities[newID][quality] = t
}

// Add a new listening track for either a video tile (initially at quality 0) or audio
func addNewTrackforPeer(pcState *peerConnectionState, trackID string) {
	v := strings.Split(string(trackID), "_")
	if v[0] == "audio" {
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
	//fmt.Printf("WebRTCSFU: [Client #%d] setTrackQuality: Implementing decision for trackID %s to use quality %d\n", pcState.ID, trackID, quality)

	defer func() {
		qualitiesLock.Unlock()
		listLock.Unlock()
	}()

	if oldQuality, keyExists := pcState.qualityDecisions[trackID]; keyExists {
		if oldQuality != quality {
			//fmt.Printf("WebRTCSFU: [Client #%d] setTrackQuality: old quality %d differs from requested quality %d\n", pcState.ID, oldQuality, quality)
			trackQuality := trackQualities[trackID][quality]
			//fmt.Printf("WebRTCSFU: [Client #%d] setTrackQuality: Received correct quality track\n", pcState.ID)
			if rtpSender, keyExists := pcState.trackRTPSenders[trackID]; keyExists {
				if rtpSender == nil {
					fmt.Printf("WebRTCSFU: [Client #%d] rtpSender NIL\n", pcState.ID)
				}
				if trackQuality == nil {
					//if pcState.ID == 0 {
					fmt.Printf("WebRTCSFU: [Client #%d] trackQuality NIL\n", pcState.ID)
					fmt.Printf("WebRTCSFU: [Client #%d] setTrackQuality: Replacing track with NIL\n", pcState.ID)
					//}

					rtpSender.ReplaceTrack(nil)
				} else {
					//if pcState.ID == 0 {
					fmt.Printf("WebRTCSFU: [Client #%d] setTrackQuality: Replacing track with new track ID %s StreamID %s %d\n", pcState.ID, trackQuality.ID(), trackQuality.StreamID(), quality)
					//}
					rtpSender.ReplaceTrack(trackQuality)
				}
			}
			//fmt.Printf("WebRTCSFU: [Client #%d] setTrackQuality: Updating quality decision to %d\n", pcState.ID, quality)
			pcState.qualityDecisions[trackID] = quality
		}
	} else {
		fmt.Printf("WebRTCSFU: [Client #%d] setTrackQuality: trackID %s not found\n", pcState.ID, trackID)
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

				fmt.Printf("WebRTCSFU: [Client #%d] attemptSync: Existing sender: %s\n", peerConnections[i].ID, sender.Track().ID())

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
						fmt.Printf("WebRTCSFU: [Client #%d] peerConnection.OnTrack: clientID %s cannot be converted to an integer\n", peerConnections[i].ID, v[1])
						panic(err)
					}
					tileID, err := strconv.Atoi(v[2])
					if err != nil {
						fmt.Printf("WebRTCSFU: [Client #%d] peerConnection.OnTrack: tileID %s cannot be converted to an integer\n", peerConnections[i].ID, v[2])
						panic(err)
					}
					trackID = fmt.Sprintf("video_%d_%d", clientID, tileID)
				}

				fmt.Printf("WebRTCSFU: [Client #%d] attemptSync: Existing retriever: %s\n", peerConnections[i].ID, trackID)
				existingSenders[trackID] = true
			}

			// Add all tracks we aren't sending yet to the PeerConnection
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					addNewTrackforPeer(&peerConnections[i], trackID)
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

			fmt.Printf("WebRTCSFU: [Client #%d] attemptSync: Sending offer to peerConnection\n", peerConnections[i].ID)

			s := fmt.Sprintf("%d@%d@%s", 0, 2, string(payload))
			wsLock.Lock()
			peerConnections[i].websocket.WriteMessage(websocket.TextMessage, []byte(s))
			wsLock.Unlock()
		}

		return
	}

	fmt.Println("WebRTCSFU: [All clients] signalPeerConnections: attempting sync of available tracks")

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
	for k, v := range r.URL.Query() {
		fmt.Printf("%s %s \n", k, v)
	}
	numberOfTiles := *maxNumberOfTiles
	numberOfTilesS := r.URL.Query().Get("ntiles")
	if numberOfTilesS != "" {
		i, err := strconv.Atoi(numberOfTilesS)
		if err == nil {
			numberOfTiles = i
		}
	}
	listLock.Lock()
	currentPCID := pcID
	pcID++
	listLock.Unlock()
	fmt.Printf("WebRTCSFU: [Address %s] webSocketHandler: New connection received, assigning Client ID #%d\n", r.RemoteAddr, currentPCID)
	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Websocket handler started\n", currentPCID)

	// Upgrade HTTP request to Websocket
	unsafeWebSocketConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: ERROR: %s\n", currentPCID, err)
		return
	}

	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Websocket handler upgraded\n", currentPCID)

	webSocketConnection := &threadSafeWriter{unsafeWebSocketConn, sync.Mutex{}}
	// When this frame returns close the Websocket
	defer func() {
		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Closing a ThreadSafeWriter\n", currentPCID)
		webSocketConnection.Close()
	}()

	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Creating a new peer connection\n", currentPCID)

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
	// peerConnection, err := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(interceptorRegistry), webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Peer connection created\n", currentPCID)

	// When this frame returns close the PeerConnection
	defer func() {
		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Closing a peer connection\n", currentPCID)
		peerConnection.Close()
	}()

	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Adding audio track\n", currentPCID)
	if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: ERROR: %s when adding audio transceiver\n", currentPCID, err)
		return
	}

	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Iterating and adding %d video tracks\n", currentPCID, numberOfTiles)
	for i := 0; i < numberOfTiles; i++ {
		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Iterating over fake tile %d\n", currentPCID, i)
		if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: ERROR: %s when adding video transceiver\n", currentPCID, err)
			return
		}

	}

	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Waiting for lock to add connection to connection list\n", currentPCID)

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

		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: OnICECandidate: New candidate addr %s port %d\n", pcState.ID, i.Address, i.Port)
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
		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: OnConnectionStateChange: Peer connection state has changed to %s\n", pcState.ID, p.String())
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: ERROR: %s\n", pcState.ID, err)
			}
		case webrtc.PeerConnectionStateClosed:
			fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: OnConnectionStateChange: Closed\n", pcState.ID)
			signalPeerConnections()
		case webrtc.PeerConnectionStateConnected:
			fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: OnConnectionStateChange: Connected\n", pcState.ID)
		}
	})

	// Called when the packet is sent over this track
	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {

		fmt.Printf("WebRTCSFU: [Client #%d] OnTrack: ID %s, StreamID %s\n", pcState.ID, t.ID(), t.StreamID())

		var trackLocal *webrtc.TrackLocalStaticRTP
		var err error
		v := strings.Split(string(t.ID()), "_")
		isPrint := false
		if v[0] == "audio" {
			trackLocal, err = webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
			if err != nil {
				panic(err)
			}
			addTrack(&pcState, trackLocal)
			defer func() {
				fmt.Printf("WebRTCSFU: [Client #%d] OnTrack: Removing track %s\n", pcState.ID, trackLocal.ID())
				removeTrack(&pcState, trackLocal)
			}()
		} else {
			clientID, err := strconv.Atoi(v[1])
			if err != nil {
				fmt.Printf("WebRTCSFU: [Client #%d] peerConnection.OnTrack: clientID %s cannot be converted to an integer\n", pcState.ID, v[1])
				panic(err)
			}
			tileID, err := strconv.Atoi(v[2])
			if err != nil {
				fmt.Printf("WebRTCSFU: [Client #%d] peerConnection.OnTrack: tileID %s cannot be converted to an integer\n", pcState.ID, v[2])
				panic(err)
			}
			quality, err := strconv.Atoi(v[3])
			if err != nil {
				fmt.Printf("WebRTCSFU: [Client #%d] peerConnection.OnTrack: quality %s cannot be converted to an integer\n", pcState.ID, v[13])
				panic(err)
			}
			if quality == 1 {
				isPrint = true
			}
			newID := fmt.Sprintf("video_%d_%d", clientID, tileID)
			newStreamID := fmt.Sprintf("%d", tileID)
			fmt.Printf("WebRTCSFU: [Client #%d] OnTrack: %s %s\n", pcState.ID, newID, newStreamID)
			trackLocal, err = webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, newID, newStreamID)
			// trackLocal, err = webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
			// trackLocal, err = webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, newID, t.StreamID())
			//	trackLocal, err = webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), newStreamID)
			if err != nil {
				panic(err)
			}
			addQualityTrack(&pcState, trackLocal, quality, newID)
			if quality == 0 {
				addTrack(&pcState, trackLocal)
				defer func() {
					fmt.Printf("WebRTCSFU: [Client #%d] OnTrack: Removing track %s\n", pcState.ID, trackLocal.ID())
					removeTrack(&pcState, trackLocal)
				}()
			}
		}
		buf := make([]byte, 1500)
		for {

			i, _, err := t.Read(buf)
			if isPrint {
				//println("test")
			}
			if err != nil {
				fmt.Printf("WebRTCSFU: [Client #%d] OnTrack: Track %s error during read: %s\n", pcState.ID, trackLocal.ID(), err)
				break
			}
			if _, err = trackLocal.Write(buf[:i]); err != nil {
				fmt.Printf("WebRTCSFU: [Client #%d] OnTrack: Track %s error during write: %s\n", pcState.ID, trackLocal.ID(), err)
				break
			}
		}
	})

	fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Will now call signalpeerconnections again\n", pcState.ID)
	signalPeerConnections()

	for {
		_, raw, err := webSocketConnection.ReadMessage()
		if err != nil {
			fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: ReadMessage: error %s\n", pcState.ID, err.Error())
			break
		}
		v := strings.Split(string(raw), "@")
		messageType, _ := strconv.ParseUint(v[1], 10, 64)
		message := v[2]
		fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Message type: %d (%s)\n", pcState.ID, messageType, websocketMessageTypeToString(messageType))
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
			fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Implementing quality decisions (%s)\n", pcState.ID, message)
			v := strings.Split(message, ";")
			for _, w := range v {
				x := strings.Split(w, ",")
				clientID := x[0]
				for tileID, sQuality := range x[1:] {
					trackID := fmt.Sprintf("video_%s_%d", clientID, tileID)
					quality, err := strconv.Atoi(sQuality)
					if err != nil {
						fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: %s cannot be converted to an integer\n", pcState.ID, sQuality)
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
