package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	FramePacketType   uint32 = 0
	TilePacketType    uint32 = 1
	ControlPacketType uint32 = 2
)

type RemoteInputPacketHeader struct {
	Framenr     uint32
	Tilenr      uint32
	Tilelen     uint32
	Frameoffset uint32
	Packetlen   uint32
}

type RemoteFrame struct {
	currentLen uint32
	frameLen   uint32
	frameData  []byte
}

type RemoteTile struct {
	currentLen uint32
	fileLen    uint32
	fileData   []byte
}

type ProxyConnection struct {
	// General
	addr *net.UDPAddr
	conn *net.UDPConn

	// Receiving
	m                 sync.RWMutex
	incomplete_frames map[uint32]RemoteFrame
	complete_frames   []RemoteFrame
	frameCounter      uint32

	incomplete_tiles map[uint32]map[uint32]RemoteTile
	complete_tiles   map[uint32][]RemoteTile
	frame_counters   map[uint32]uint32

	send_mutex sync.Mutex
}

type SetupCallback func(int)

func NewProxyConnection() *ProxyConnection {
	return &ProxyConnection{nil, nil, sync.RWMutex{}, make(map[uint32]RemoteFrame), make([]RemoteFrame, 0), 0,
		make(map[uint32]map[uint32]RemoteTile), make(map[uint32][]RemoteTile), make(map[uint32]uint32), sync.Mutex{}}
}

func (pc *ProxyConnection) sendPacket(b []byte, offset uint32, packet_type uint32) {
	buffProxy := make([]byte, 1300)
	binary.LittleEndian.PutUint32(buffProxy[0:], packet_type)
	copy(buffProxy[4:], b[offset:])

	pc.send_mutex.Lock()
	_, err := pc.conn.WriteToUDP(buffProxy, pc.addr)
	pc.send_mutex.Unlock()

	if err != nil {
		fmt.Println("Error sending response:", err)
		panic(err)
	}
}

func (pc *ProxyConnection) SetupConnection(port string, cb SetupCallback) {
	address, err := net.ResolveUDPAddr("udp", port)
	if err != nil {
		fmt.Println("Error resolving address:", err)
		return
	}

	// Create a UDP connection
	pc.conn, err = net.ListenUDP("udp", address)
	if err != nil {
		fmt.Println("Error listening:", err)
		return
	}

	// Create a buffer to read incoming messages
	buffer := make([]byte, 1500)

	// Wait for incoming messages
	fmt.Println("Waiting for a message...")
	_, pc.addr, err = pc.conn.ReadFromUDP(buffer)
	if err != nil {
		fmt.Println("Error reading:", err)
		return
	}

	// Extract the number of tiles
	numberOfTiles := int(buffer[0])
	fmt.Printf("Starting WebRTC with %d tiles\n", numberOfTiles)

	// Call the callback
	cb(numberOfTiles)

	fmt.Println("Connected to proxy")
}

func (pc *ProxyConnection) StartListening() {
	println("Start listening")
	go func() {
		for {
			buffer := make([]byte, 1500)
			_, _, _ = pc.conn.ReadFromUDP(buffer)
			bufBinary := bytes.NewBuffer(buffer[4:24])
			// Read the fields from the buffer into a struct
			var p RemoteInputPacketHeader
			err := binary.Read(bufBinary, binary.LittleEndian, &p)
			if err != nil {
				fmt.Println("Error:", err)
				fmt.Println("QUITING")
				return
			}

			pc.m.Lock()
			_, exists := pc.incomplete_tiles[p.Tilenr]
			if !exists {
				pc.incomplete_tiles[p.Tilenr] = make(map[uint32]RemoteTile)
			}
			_, exists = pc.incomplete_tiles[p.Tilenr][p.Framenr]
			if !exists {
				r := RemoteTile{
					0,
					p.Tilelen,
					make([]byte, p.Tilelen),
				}
				pc.incomplete_tiles[p.Tilenr][p.Framenr] = r
			}
			value := pc.incomplete_tiles[p.Tilenr][p.Framenr]
			copy(value.fileData[p.Frameoffset:p.Frameoffset+p.Packetlen], buffer[24:24+p.Packetlen])
			value.currentLen = value.currentLen + p.Packetlen
			pc.incomplete_tiles[p.Tilenr][p.Framenr] = value
			if value.currentLen == value.fileLen {
				if p.Framenr%100 == 0 {
					println("Received frame ", p.Framenr, " - tile ", p.Tilenr)
				}
				_, exists := pc.complete_tiles[p.Tilenr]
				if !exists {
					pc.complete_tiles[p.Tilenr] = make([]RemoteTile, 0)
				}
				pc.complete_tiles[p.Tilenr] = append(pc.complete_tiles[p.Tilenr], value)
				delete(pc.incomplete_tiles[p.Tilenr], p.Framenr)
			}
			pc.m.Unlock()
		}
	}()
}

/*
_, exists := pc.incomplete_frames[p.Framenr]
	if !exists {
		r := RemoteFrame{
			0,
			p.Tilelen,
			make([]byte, p.Tilelen),
		}
		pc.incomplete_frames[p.Framenr] = r
	}
	value := pc.incomplete_frames[p.Framenr]

	copy(value.frameData[p.Frameoffset:p.Frameoffset+p.Packetlen], buffer[24:24+p.Packetlen])
	value.currentLen = value.currentLen + p.Packetlen
	pc.incomplete_frames[p.Framenr] = value
	if value.currentLen == value.frameLen {
		if p.Framenr%100 == 0 {
			println("FRAME ", p.Framenr, " RECEIVED FROM UNITY")
		}
		pc.complete_frames = append(pc.complete_frames, value)
		delete(pc.incomplete_frames, p.Framenr)
	}
*/

func (pc *ProxyConnection) SendFramePacket(b []byte, offset uint32) {
	pc.sendPacket(b, offset, FramePacketType)
}

func (pc *ProxyConnection) SendTilePacket(b []byte, offset uint32) {
	pc.sendPacket(b, offset, TilePacketType)
}

func (pc *ProxyConnection) SendControlPacket(b []byte) {
	pc.sendPacket(b, 0, ControlPacketType)
}

func (pc *ProxyConnection) NextFrame() []byte {
	isNextFrameReady := false
	for !isNextFrameReady {
		pc.m.Lock()
		if len(pc.complete_frames) > 0 {
			isNextFrameReady = true
		}
		pc.m.Unlock()
		time.Sleep(time.Millisecond)
	}
	pc.m.Lock()
	data := pc.complete_frames[0].frameData
	if pc.frameCounter%100 == 0 {
		println("SENDING FRAME ", pc.frameCounter)
	}
	pc.complete_frames = pc.complete_frames[1:]
	pc.frameCounter = pc.frameCounter + 1
	pc.m.Unlock()
	return data
}

func (pc *ProxyConnection) NextTile(tile uint32) []byte {
	isNextFrameReady := false
	for !isNextFrameReady {
		pc.m.Lock()
		_, exists := pc.complete_tiles[tile]
		if !exists {
			pc.complete_tiles[tile] = make([]RemoteTile, 0)
		}
		if len(pc.complete_tiles[tile]) > 0 {
			isNextFrameReady = true
		}
		pc.m.Unlock()
		time.Sleep(time.Millisecond)
	}
	pc.m.Lock()
	data := pc.complete_tiles[tile][0].fileData
	if pc.frame_counters[tile]%100 == 0 {
		println("Sending frame", pc.frame_counters[tile], "- tile", tile)
	}
	pc.complete_tiles[tile] = pc.complete_tiles[tile][1:]
	pc.frame_counters[tile] += 1
	pc.m.Unlock()
	return data
}
