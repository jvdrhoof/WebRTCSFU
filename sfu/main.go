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

	"golang.org/x/exp/slices"

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

	// lock for peerConnections and trackLocals
	listLock           sync.RWMutex
	peerConnections    []peerConnectionState
	trackLocals        map[string]*webrtc.TrackLocalStaticRTP
	settingEngine      webrtc.SettingEngine
	wsLock             sync.RWMutex
	maxNumberOfTiles   *int
	undesireableTracks map[int][]string
	pcID               = 0
)

type WebsocketPacket struct {
	ClientID    uint64
	MessageType uint64
	Message     string
}

type peerConnectionState struct {
	peerConnection  *webrtc.PeerConnection
	websocket       *threadSafeWriter
	ID              int
	trackRTPSenders map[string]*webrtc.RTPSender

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
	undesireableTracks = map[int][]string{}

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

// Add to list of tracks and fire renegotation for all PeerConnections
func addTrack(pcState *peerConnectionState, t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		fmt.Printf("WebRTCSFU: [Client #%d] addTrack: Calling signalPeerConnections to inform other clients\n", pcState.ID)
		signalPeerConnections()
	}()

	fmt.Printf("WebRTCSFU: [Client #%d] addTrack: t.ID %s, t.StreamID %s\n", pcState.ID, t.ID(), t.StreamID())

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	trackLocals[t.ID()] = trackLocal
	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections
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

func addTrackforPeer(pcState peerConnectionState, trackID string) {
	trackLocal := trackLocals[trackID]
	if rtpSender, keyExists := pcState.trackRTPSenders[trackID]; keyExists {
		if rtpSender.Track() == nil || rtpSender.Track().ID() != trackID {
			rtpSender.ReplaceTrack(trackLocal)
		}
	} else {
		var sender *webrtc.RTPSender
		var err error
		if sender, err = pcState.peerConnection.AddTrack(trackLocal); err != nil {
			panic(err)
		}
		pcState.trackRTPSenders[trackID] = sender
	}

	v := undesireableTracks[pcState.ID]
	for i, t := range v {
		if t == trackID {
			v = append(v[:i], v[i+1:]...)
			break
		}
	}
	undesireableTracks[pcState.ID] = v
}

func removeTrackforPeer(pcState peerConnectionState, trackID string) {
	if rtpSender, exists := pcState.trackRTPSenders[trackID]; exists {
		rtpSender.ReplaceTrack(nil)
		// TODO Maybe send rtcp packet to client to inform him that track is now replaced? i.e. use rtpSender.Transport().WriteRTCP()
		//undesireableTracks[pcState.ID] = append(undesireableTracks[pcState.ID], trackID)
	}
	/*for _, sender := range pcState.peerConnection.GetSenders() {
		if sender.Track().ID() == trackID {
			pcState.peerConnection.RemoveTrack(sender)
			undesireableTracks[pcState.ID] = append(undesireableTracks[pcState.ID], trackID)
			break
		}
	}*/
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func receiveTracks(pcState peerConnectionState, trackIDs []string) {
	//v := undesireableTracks[pcState.ID]
	for senderId, sender := range pcState.trackRTPSenders {
		if sender.Track() != nil && sender.Track().Kind() == webrtc.RTPCodecTypeAudio {
			continue
		}
		hasAdded := false
		for _, id := range trackIDs {
			if senderId == id {
				addTrackforPeer(pcState, id)
				hasAdded = true
				break
			}
		}
		if !hasAdded {
			removeTrackforPeer(pcState, senderId)
		}
	}
	/*for _, t := range trackIDs {
		if sender, exists := pcState.trackRTPSenders[t]; exists {
			if sender.Track() == nil {
				if track, trackExists := trackLocals[t]; trackExists {
					sender.ReplaceTrack(track)
				}
			}
		}
	}
	for _, sender := range pcState.peerConnection.GetSenders() {
		trackID := sender.Track().ID()
		if contains(trackIDs, trackID) {
			if contains(v, trackID) {
				addTrackforPeer(pcState, trackID)
			}
		} else {
			if !contains(v, trackID) {
				removeTrackforPeer(pcState, trackID)
			}
		}
	}*/
}

// TODO does this work with multiple tiles / audio?
// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks
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

			// map of sender we already are seanding, so we don't double send
			existingSenders := map[string]bool{}

			for _, sender := range peerConnections[i].peerConnection.GetSenders() {
				if sender.Track() == nil {
					continue
				}

				existingSenders[sender.Track().ID()] = true

				// If we have a RTPSender that doesn't map to a existing track remove and signal
				if _, ok := trackLocals[sender.Track().ID()]; !ok {
					// TODO remove from undesireable track if exists?
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

				existingSenders[receiver.Track().ID()] = true
			}

			// Add all track we aren't sending yet to the PeerConnection
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					if !slices.Contains(undesireableTracks[peerConnections[i].ID], trackID) {
						addTrackforPeer(peerConnections[i], trackID)
						/*if _, err := peerConnections[i].peerConnection.AddTrack(trackLocals[trackID]); err != nil {
							return true
						}*/
					}
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

			/*offerString, err := json.Marshal(offer)
			if err != nil {
				return true
			}

			if err = peerConnections[i].websocket.WriteJSON(&websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}*/
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
	// TODO Audio RTP
	videoCodecCapability := webrtc.RTPCodecCapability{
		MimeType:     "video/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: videoRTCPFeedback,
	}

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
	mediaEngine.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	mediaEngine.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeVideo)
	if err := mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

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

	peerConnection, err := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
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
	var pcState = peerConnectionState{peerConnection, webSocketConnection, currentPCID, make(map[string]*webrtc.RTPSender), make([]string, 0)}
	//pcID += 1
	peerConnections = append(peerConnections, pcState)
	//fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: peerConnection \n", pcState.ID)
	undesireableTracks[currentPCID] = []string{}
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

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		//if t.Kind() == webrtc.RTPCodecTypeAudio {
		//	return
		//}
		trackLocal := addTrack(&pcState, t)
		defer func() {
			fmt.Printf("WebRTCSFU: [Client #%d] OnTrack: Removing track %s\n", pcState.ID, trackLocal.ID())
			removeTrack(&pcState, trackLocal)
		}()

		buf := make([]byte, 1500)
		for {
			i, _, err := t.Read(buf)
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

	// Signal for the new PeerConnection
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
		// answer
		case 3:
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
		// candidate
		case 4:
			desc := peerConnection.RemoteDescription()
			if desc == nil {
				pcState.pendingCandidatesString = append(pcState.pendingCandidatesString, message)
			} else {
				if candidateErr := peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: message}); candidateErr != nil {
					panic(candidateErr)
				}
			}
			/*candidate := webrtc.ICECandidateInit{Candidate: message}
			if err := peerConnection.AddICECandidate(candidate); err != nil {
				panic(err)
			}*/
		// remove track
		case 5:
			removeTrackforPeer(pcState, message)
		// add track
		case 6:
			addTrackforPeer(pcState, message)
		// tile qualities
		case 7:
			fmt.Printf("WebRTCSFU: [Client #%d] webSocketHandler: Setting tile priorities (%s)\n", pcState.ID, message)
			var trackIDs []string
			v := strings.Split(message, ";")
			for _, w := range v {
				x := strings.Split(w, ",")
				clientID := x[0]
				for _, tileID := range x[1:] {
					trackID := fmt.Sprintf("video_%s_%s", clientID, tileID)
					trackIDs = append(trackIDs, trackID)
				}
			}
			receiveTracks(pcState, trackIDs)
		}
	}
}

func websocketMessageTypeToString(messageType uint64) string {
	switch messageType {
	// answer
	case 3:
		return "Answer with Remote Description"
	// candidate
	case 4:
		return "New ICE Candidate"
	// remove track
	case 5:
		return "Remove Track"
	// add track
	case 6:
		return "Add Track"
	// tile priorities
	case 7:
		return "Tile priorities"
	}
	return "Unknown Message Type"
}

// Helper to make Gorilla Websockets threadsafe
type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

/*
func (t *threadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}
*/
