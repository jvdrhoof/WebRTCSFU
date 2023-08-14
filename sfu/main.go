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
	numberOfTiles      *int
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
	// Parse the flags passed to program
	numberOfTiles = flag.Int("t", 1, "Number of tiles.")
	flag.Parse()

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetSCTPMaxReceiveBufferSize(16 * 1024 * 1024)

	// Init other state
	log.SetFlags(0)
	trackLocals = map[string]*webrtc.TrackLocalStaticRTP{}
	undesireableTracks = map[int][]string{}

	// Read index.html from disk into memory, serve whenever anyone requests /
	indexHTML, err := ioutil.ReadFile("index.html")
	if err != nil {
		panic(err)
	}
	indexTemplate = template.Must(template.New("").Parse(string(indexHTML)))

	// websocket handler
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
		println("Calling signalPeerConnections from addTrack")
		signalPeerConnections()
	}()

	// Print ID and StreamID
	println("ID's: ", t.ID(), t.StreamID())

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
		signalPeerConnections()
	}()

	delete(trackLocals, t.ID())
}

func addTrackforPeer(pcState peerConnectionState, trackID string) {
	trackLocal := trackLocals[trackID]
	if _, err := pcState.peerConnection.AddTrack(trackLocal); err != nil {
		panic(err)
	}
	// Update preferences
	v := undesireableTracks[pcState.ID]
	for i, t := range v {
		if t == trackID {
			v = append(v[:i], v[i+1:]...)
			break
		}
	}
	// TODO: might not be needed
	undesireableTracks[pcState.ID] = v
}

func removeTrackforPeer(pcState peerConnectionState, trackID string) {
	for _, sender := range pcState.peerConnection.GetSenders() {
		if sender.Track().ID() == trackID {
			pcState.peerConnection.RemoveTrack(sender)
			// Update preferences
			undesireableTracks[pcState.ID] = append(undesireableTracks[pcState.ID], trackID)
			break
		}
	}
}

// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks
func signalPeerConnections() {

	println("Signal peer connections")

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

			println("Sending offer")

			s := fmt.Sprintf("%d@%d@%s", 1, 2, string(payload))
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

		println("Return from attemptSync")

		return
	}

	println("Entering syncAttempt")

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 1 { // Why try 25 times?
			// Release the lock and attempt a sync in 3 seconds. We might be blocking a RemoveTrack or AddTrack
			go func() {
				time.Sleep(time.Second * 5) // Why every 3 seconds?
				signalPeerConnections()
			}()
			return
		}

		if !attemptSync() {
			break
		}
	}

	println("Return from signalPeerConnections")
}

// Handle incoming websockets
func websocketHandler(w http.ResponseWriter, r *http.Request) {

	println("Websocket handler started")

	// Upgrade HTTP request to Websocket
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		println("upgrade:", err)
		return
	}

	println("Websocket handler upgraded")

	c := &threadSafeWriter{unsafeConn, sync.Mutex{}}

	// When this frame returns close the Websocket
	defer func() {
		println("Closing a ThreadSafeWriter")
		c.Close()
	}()

	println("Creating a new peer connection")

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

	codecCapability := webrtc.RTPCodecCapability{
		MimeType:     "video/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: videoRTCPFeedback,
	}

	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: codecCapability,
		PayloadType:        5,
	}, webrtc.RTPCodecTypeVideo); err != nil {
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

	println("Peer connection created")

	// When this frame returns close the PeerConnection
	defer func() {
		println("Closing a peer connection")
		peerConnection.Close()
	}()

	println("Iterating over video tracks")

	for i := 0; i < *numberOfTiles; i++ {
		if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			fmt.Println(err)
			return
		}
	}

	println("Waiting for lock")

	// Add our new PeerConnection to global list
	listLock.Lock()
	println("Appending!")
	var pcState = peerConnectionState{peerConnection, c, pcID}
	pcID += 1
	peerConnections = append(peerConnections, pcState)
	undesireableTracks[pcID] = []string{}
	listLock.Unlock()

	// Trickle ICE. Emit server candidate to client
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		println("Found a candidate")
		payload := []byte(i.ToJSON().Candidate)
		s := fmt.Sprintf("%d@%d@%s", 1, 4, string(payload))
		wsLock.Lock()
		err = c.WriteMessage(websocket.TextMessage, []byte(s))
		wsLock.Unlock()
		if err != nil {
			panic(err)
		}
	})

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		fmt.Printf("Peer connection state has changed: %s\n", p.String())
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				println(err)
			}
		case webrtc.PeerConnectionStateClosed:
			signalPeerConnections()
		case webrtc.PeerConnectionStateConnected:
			println("Connected!")
			/*for ; true; <-time.NewTicker(1000 * time.Millisecond).C {
				// TODO: extract bitrate here
				// targetBitrate := uint32(0)
				// println("Target bitrate equals", targetBitrate)
			}*/
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		trackLocal := addTrack(t)
		defer removeTrack(trackLocal)

		buf := make([]byte, 1500)
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				panic(err)
			}

			if _, err = trackLocal.Write(buf[:i]); err != nil {
				panic(err)
			}
		}
	})

	println("Will now call signalpeerconnections again")

	// Signal for the new PeerConnection
	signalPeerConnections()

	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			panic(err)
		}
		v := strings.Split(string(raw), "@")
		messageType, _ := strconv.ParseUint(v[1], 10, 64)
		message := v[2]
		println("Message type: ", messageType)
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

func (t *threadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}
