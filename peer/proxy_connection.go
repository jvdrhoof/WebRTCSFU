package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

const (
	ReadyPacketType   uint32 = 0
	TilePacketType    uint32 = 1
	AudioPacketType   uint32 = 2
	ControlPacketType uint32 = 3
)

// TODO seperate this into different struct. We also want to use packet type for control packets (i.e. fov)
type RemoteInputPacketHeader struct {
}

// TODO Refactor this
type RemoteInputVideoPacketHeader struct {
	ClientNr    uint32
	FrameNr     uint32
	FrameLen    uint32
	FrameOffset uint32
	PacketLen   uint32
	TileNr      uint32
	Quality     uint32
}

type VideoKey struct {
	TileNr  uint32
	Quality uint32
}

type RemoteInputAudioPacketHeader struct {
	// TODO: do we have special audio fields?
	ClientNr    uint32
	FrameNr     uint32
	FrameLen    uint32
	FrameOffset uint32
	PacketLen   uint32
}

// TODO split audio and video? Technically can both use this struct
type RemoteTile struct {
	frameNr    uint32
	currentLen uint32
	fileLen    uint32
	fileData   []byte
}

type ProxyConnection struct {
	addr             *net.UDPAddr
	conn             *net.UDPConn
	m                PriorityLock
	incomplete_tiles map[VideoKey]map[uint32]RemoteTile // We probably want to limit the max number of incomplete tiles?
	//And maybe use something else than a simple map because atm there is technically a max frame limit
	complete_tiles map[VideoKey][]RemoteTile

	incomplete_audio_frames map[uint32]RemoteTile
	complete_audio_frames   []RemoteTile

	send_mutex sync.Mutex

	// Cond gives better performance compared to high priority lock
	// High prio lock => latency between 3 and 10ms
	// Condi lock => latency between 0 and 1ms
	cond_video map[VideoKey]*sync.Cond
	mtx_video  sync.Mutex

	cond_audio *sync.Cond
	mtx_audio  sync.Mutex

	ws_handler *WebsocketHandler
}

type SetupCallback func(int)

func NewProxyConnection() *ProxyConnection {
	return &ProxyConnection{nil, nil, NewPriorityPreferenceLock(),
		make(map[VideoKey]map[uint32]RemoteTile), make(map[VideoKey][]RemoteTile), // Video
		make(map[uint32]RemoteTile), make([]RemoteTile, 0), // Audio
		sync.Mutex{},                                // Send mutex
		make(map[VideoKey]*sync.Cond), sync.Mutex{}, // Video mutex
		nil, sync.Mutex{}, // Audio mutex
		nil,
	}
}

func (pc *ProxyConnection) sendPacket(b []byte, offset uint32, packet_type uint32) {
	buffProxy := make([]byte, BufferProxyLength)
	binary.LittleEndian.PutUint32(buffProxy[0:], packet_type)
	copy(buffProxy[TypeHeaderSize:], b[offset:])
	pc.send_mutex.Lock()
	_, err := pc.conn.WriteToUDP(buffProxy, pc.addr)
	pc.send_mutex.Unlock()
	if err != nil {
		logger.Error("sendPacket", fmt.Sprintf("%s", err))
		panic(err)
	}
}

func (pc *ProxyConnection) SetupConnection(port string) {
	address, err := net.ResolveUDPAddr("udp", port)
	if err != nil {
		logger.Error("SetupConnection", fmt.Sprintf("%s", err))
		return
	}

	// Create a UDP connection
	pc.conn, err = net.ListenUDP("udp", address)
	if err != nil {
		logger.Error("SetupConnection", fmt.Sprintf("%s", err))
		return
	}

	// Create a buffer to read incoming messages
	port_info := strings.Split(port, ":")
	if port_info[0] == "" {
		port_int, _ := strconv.Atoi(port_info[1])
		port_string := strconv.Itoa(port_int + 1)
		port = "127.0.0.1:" + port_string
	} else {
		port_int, _ := strconv.Atoi(port_info[1])
		port_string := strconv.Itoa(port_int + 1)
		port = port_info[0] + ":" + port_string
	}

	pc.addr, err = net.ResolveUDPAddr("udp", port)
	if err != nil {
		logger.Error("SetupConnection", fmt.Sprintf("%s", err))
		return
	}

	pc.SendPeerReadyPacket()
	buffer := make([]byte, UDPBufferLength)

	// Wait for incoming messages
	logger.Log("SetupConnection", fmt.Sprintf("Waiting for a message on port %s and IP address %s", port, pc.addr.IP.String()), LevelVerbose)
	_, pc.addr, err = pc.conn.ReadFromUDP(buffer)
	if err != nil {
		logger.Error("SetupConnection", fmt.Sprintf("%s", err))
		return
	}
	logger.Log("SetupConnection", "Connected to Unity DLL", LevelVerbose)
}

func (pc *ProxyConnection) StartListening(nTiles int, nQualities int) {
	logger.Log("StartListening", "Start listening for incoming data from DLL", LevelVerbose)
	for t := 0; t < nTiles; t++ {
		for q := 0; q < nQualities; q++ {
			pc.cond_video[VideoKey{uint32(t), uint32(q)}] = sync.NewCond(&pc.mtx_video)
		}
	}
	pc.cond_audio = sync.NewCond(&pc.mtx_audio)
	go func() {
		for {
			buffer := make([]byte, UDPBufferLength)
			_, _, _ = pc.conn.ReadFromUDP(buffer)
			ptype := binary.LittleEndian.Uint32(buffer[:TypeHeaderSize])
			if ptype == TilePacketType {
				bufBinary := bytes.NewBuffer(buffer[TypeHeaderSize : TypeHeaderSize+VideoHeaderSize])
				var p RemoteInputVideoPacketHeader
				err := binary.Read(bufBinary, binary.LittleEndian, &p) // TODO: make sure we check endianess of system here and use that instead!
				if err != nil {
					logger.Error("StartListening", fmt.Sprintf("%s", err))
					return
				}

				pc.mtx_video.Lock()
				key := VideoKey{p.TileNr, p.Quality}
				_, exists := pc.incomplete_tiles[key]
				if !exists {
					pc.incomplete_tiles[key] = make(map[uint32]RemoteTile)
				}
				_, exists = pc.incomplete_tiles[key][p.FrameNr]
				if !exists {
					r := RemoteTile{
						p.FrameNr,
						0,
						p.FrameLen,
						make([]byte, p.FrameLen),
					}
					pc.incomplete_tiles[key][p.FrameNr] = r
				}
				value := pc.incomplete_tiles[key][p.FrameNr]
				copy(value.fileData[p.FrameOffset:p.FrameOffset+p.PacketLen], buffer[TypeHeaderSize+VideoHeaderSize:TypeHeaderSize+VideoHeaderSize+p.PacketLen])
				value.currentLen = value.currentLen + p.PacketLen
				pc.incomplete_tiles[key][p.FrameNr] = value
				if value.currentLen == value.fileLen {
					_, exists := pc.complete_tiles[key]
					if !exists {
						pc.complete_tiles[key] = make([]RemoteTile, 1)
					}
					// For now we will only save 1 frame for each tile max (do we want to save more?)
					// TODO: Use channels instead
					pc.complete_tiles[key][0] = value
					delete(pc.incomplete_tiles[key], p.FrameNr)
					// TODO: Check if order broadcast -> unlock is correct
					pc.cond_video[key].Broadcast()
				}
				pc.mtx_video.Unlock()
			} else if ptype == AudioPacketType {
				bufBinary := bytes.NewBuffer(buffer[TypeHeaderSize : TypeHeaderSize+AudioHeaderSize])
				var p RemoteInputAudioPacketHeader
				// TODO: Make sure we check endianess of system here and use that instead
				err := binary.Read(bufBinary, binary.LittleEndian, &p)
				if err != nil {
					logger.Error("StartListening", fmt.Sprintf("%s", err))
					return
				}
				pc.mtx_audio.Lock()
				_, exists := pc.incomplete_audio_frames[p.FrameNr]
				if !exists {
					r := RemoteTile{
						p.FrameNr,
						0,
						p.FrameLen,
						make([]byte, p.FrameLen),
					}
					pc.incomplete_audio_frames[p.FrameNr] = r
				}
				value := pc.incomplete_audio_frames[p.FrameNr]
				copy(value.fileData[p.FrameOffset:p.FrameOffset+p.PacketLen], buffer[TypeHeaderSize+AudioHeaderSize:TypeHeaderSize+AudioHeaderSize+p.PacketLen])
				value.currentLen = value.currentLen + p.PacketLen
				pc.incomplete_audio_frames[p.FrameNr] = value
				if value.currentLen == value.fileLen {
					// For now we will only save 1 frame for each tile max (do we want to save more?)
					// TODO: Use channels instead
					if len(pc.complete_audio_frames) == 0 {
						pc.complete_audio_frames = append(pc.complete_audio_frames, value)
					} else {
						pc.complete_audio_frames[0] = value
					}
					delete(pc.incomplete_audio_frames, p.FrameNr)
					pc.cond_audio.Broadcast()
				}
				pc.mtx_audio.Unlock()
			} else if ptype == ControlPacketType {
				logger.Log("StartListening", string(buffer[TypeHeaderSize:]), 0)
				if pc.ws_handler != nil {
					pc.ws_handler.SendMessage(WebsocketPacket{
						0,
						7,
						string(buffer[TypeHeaderSize:]),
					})
				}
			}
		}
	}()
}

func (pc *ProxyConnection) SendPeerReadyPacket() {
	pc.sendPacket(make([]byte, PeerReadyLength), 0, ReadyPacketType)
}

func (pc *ProxyConnection) SendTilePacket(b []byte, offset uint32) {
	pc.sendPacket(b, offset, TilePacketType)
}

func (pc *ProxyConnection) SendAudioPacket(b []byte, offset uint32) {
	pc.sendPacket(b, offset, AudioPacketType)
}

func (pc *ProxyConnection) SendControlPacket(b []byte) {
	pc.sendPacket(b, 0, ControlPacketType)
}

func (pc *ProxyConnection) NextTile(tile uint32, quality uint32) []byte {
	isNextFrameReady := false
	key := VideoKey{tile, quality}
	for !isNextFrameReady {
		pc.mtx_video.Lock()
		if len(pc.complete_tiles[key]) > 0 {
			isNextFrameReady = true
		} else {
			pc.cond_video[key].Wait()
			isNextFrameReady = true
		}
	}
	data := pc.complete_tiles[key][0].fileData
	delete(pc.complete_tiles, key)
	pc.mtx_video.Unlock()
	return data
}

func (pc *ProxyConnection) NextAudioFrame() []byte {
	isNextFrameReady := false
	for !isNextFrameReady {
		pc.mtx_audio.Lock()
		if len(pc.complete_audio_frames) > 0 {
			isNextFrameReady = true
		} else {
			pc.cond_audio.Wait()
			isNextFrameReady = true
		}
	}
	data := pc.complete_audio_frames[0].fileData
	pc.complete_audio_frames = pc.complete_audio_frames[:0]
	pc.mtx_audio.Unlock()
	return data
}
