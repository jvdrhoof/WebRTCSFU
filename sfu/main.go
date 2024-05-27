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

	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
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
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
	ID             int
}

func main() {
	maxNumberOfTiles = flag.Int("t", 1, "Number of tiles")
	flag.Parse()

	fmt.Printf("WebRTCSFU: Starting SFU with at most %d tiles per client\n", *maxNumberOfTiles)

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
func addTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		fmt.Println("WebRTCSFU: addTrack: Calling signalPeerConnections")
		signalPeerConnections()
	}()

	fmt.Printf("WebRTCSFU: addTrack: t.ID %s, t.StreamID %s\n", t.ID(), t.StreamID())

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	trackLocals[t.ID()] = trackLocal
	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections
func removeTrack(t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		fmt.Println("WebRTCSFU: removeTrack: Calling signalPeerConnections")
		signalPeerConnections()
	}()

	fmt.Printf("WebRTCSFU: removeTrack: t.ID %s\n", t.ID())
	delete(trackLocals, t.ID())
}

func addTrackforPeer(pcState peerConnectionState, trackID string) {
	trackLocal := trackLocals[trackID]
	if _, err := pcState.peerConnection.AddTrack(trackLocal); err != nil {
		panic(err)
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
	for _, sender := range pcState.peerConnection.GetSenders() {
		if sender.Track().ID() == trackID {
			pcState.peerConnection.RemoveTrack(sender)
			undesireableTracks[pcState.ID] = append(undesireableTracks[pcState.ID], trackID)
			break
		}
	}
}

// TODO does this work with multiple tiles / audio?
// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks
func signalPeerConnections() {
	fmt.Println("WebRTCSFU: signalPeerConnections")

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
						if _, err := peerConnections[i].peerConnection.AddTrack(trackLocals[trackID]); err != nil {
							return true
						}
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

			fmt.Printf("WebRTCSFU: attemptSync: Sending offer to peerConnection #%d\n", i)

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

	fmt.Println("WebRTCSFU: signalPeerConnections: attempting sync")

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

	fmt.Println("WebRTCSFU: webSocketHandler: Websocket handler started")

	// Upgrade HTTP request to Websocket
	unsafeWebSocketConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("WebRTCSFU: webSocketHandler: ERROR: %s\n", err)
		return
	}

	fmt.Println("WebRTCSFU: webSocketHandler: Websocket handler upgraded")

	webSocketConnection := &threadSafeWriter{unsafeWebSocketConn, sync.Mutex{}}

	// When this frame returns close the Websocket
	defer func() {
		fmt.Println("WebRTCSFU: webSocketHandler: Closing a ThreadSafeWriter")
		webSocketConnection.Close()
	}()

	fmt.Println("WebRTCSFU: webSocketHandler: Creating a new peer connection")

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

	congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(1_000_000))
	})
	if err != nil {
		panic(err)
	}

	estimatorChan := make(chan cc.BandwidthEstimator, 1)
	congestionController.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) {
		estimatorChan <- estimator
	})

	interceptorRegistry.Add(congestionController)
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

	fmt.Println("WebRTCSFU: webSocketHandler: Peer connection created")

	// When this frame returns close the PeerConnection
	defer func() {
		fmt.Println("WebRTCSFU: webSocketHandler: Closing a peer connection")
		peerConnection.Close()
	}()

	fmt.Println("WebRTCSFU: webSocketHandler: Iterating over video tracks")

	for i := 0; i < *maxNumberOfTiles; i++ {
		if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			fmt.Printf("WebRTCSFU: webSocketHandler: ERROR: %s\n", err)
			return
		}
	}

	fmt.Println("WebRTCSFU: webSocketHandler: Adding audio track")

	if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		fmt.Printf("WebRTCSFU: webSocketHandler: ERROR: %s\n", err)
		return
	}

	fmt.Println("WebRTCSFU: webSocketHandler: Waiting for lock")

	// Add our new PeerConnection to global list
	listLock.Lock()
	var pcState = peerConnectionState{peerConnection, webSocketConnection, pcID}
	pcID += 1
	peerConnections = append(peerConnections, pcState)
	fmt.Printf("WebRTCSFU: webSocketHandler: peerConnection #%d\n", len(peerConnections))
	undesireableTracks[pcID] = []string{}
	listLock.Unlock()

	// Trickle ICE and emit server candidate to client
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		fmt.Println("WebRTCSFU: webSocketHandler: OnICECandidate: Found a candidate")
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
		fmt.Printf("WebRTCSFU: webSocketHandler: OnConnectionStateChange: Peer connection state has changed to %s\n", p.String())
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				fmt.Printf("WebRTCSFU: webSocketHandler: ERROR: %s\n", err)
			}
		case webrtc.PeerConnectionStateClosed:
			fmt.Println("WebRTCSFU: webSocketHandler: OnConnectionStateChange: Closed")
			signalPeerConnections()
		case webrtc.PeerConnectionStateConnected:
			fmt.Println("WebRTCSFU: webSocketHandler: OnConnectionStateChange: Connected")
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		//if t.Kind() == webrtc.RTPCodecTypeAudio {
		//	return
		//}
		trackLocal := addTrack(t)
		defer func() {
			fmt.Printf("WebRTCSFU: OnTrack: removing track %w\n", trackLocal.ID)
			removeTrack(trackLocal)
		}()

		buf := make([]byte, 1500)
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				fmt.Printf("WebRTCSFU: OnTrack: error during read: %s\n", err)
				break
			}

			if _, err = trackLocal.Write(buf[:i]); err != nil {
				fmt.Printf("WebRTCSFU: OnTrack: error during write: %s\n", err)
				break
			}
		}
	})

	fmt.Println("WebRTCSFU: webSocketHandler: Will now call signalpeerconnections again")

	// Signal for the new PeerConnection
	signalPeerConnections()

	for {
		_, raw, err := webSocketConnection.ReadMessage()
		if err != nil {
			fmt.Printf("WebRTCSFU: webSocketHandler: ReadMessage: error %w\n", err)
			break
		}
		v := strings.Split(string(raw), "@")
		messageType, _ := strconv.ParseUint(v[1], 10, 64)
		message := v[2]
		fmt.Printf("WebRTCSFU: webSocketHandler: Message type: %d\n", messageType)
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
		// candidate
		case 4:
			candidate := webrtc.ICECandidateInit{Candidate: message}
			if err := peerConnection.AddICECandidate(candidate); err != nil {
				panic(err)
			}
		// remove track
		case 5:
			removeTrackforPeer(pcState, message)
		// add track
		case 6:
			addTrackforPeer(pcState, message)
		}
	}
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
