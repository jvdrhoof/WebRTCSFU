package main

import (
	"encoding/binary"
	"fmt"
)

const TypeHeaderSize = 4
const WebRTCHeaderSize = 20
const WebRTCPayloadSize = 1172
const RTPPacketizerMTU = 1200
const BufferLength = 1220
const RTCPBufferLength = 1500
const UDPBufferLength = 1500
const PeerReadyLength = 100
const BufferProxyLength = 1300

type FramePacketHeader struct {
	ClientNr  uint32 // 4
	FrameNr   uint32 // 8
	FrameLen  uint32 // 12
	SeqOffset uint32 // 16
	SeqLen    uint32 // 20
}

const VideoHeaderSize = 28
const VideoPayloadSize = WebRTCPayloadSize - VideoHeaderSize

type VideoFramePacket struct {
	FramePacketHeader        // 20
	TileNr            uint32 // 24
	Quality           uint32 // 28
	Data              [VideoPayloadSize]byte
}

const AudioHeaderSize = 20
const AudioPayloadSize = WebRTCPayloadSize - AudioHeaderSize

type AudioFramePacket struct {
	FramePacketHeader // 20
	Data              [AudioPayloadSize]byte
}

// Point cloud payloader
type PointCloudPayloader struct {
	frameCounter uint32
	tile         uint32
	quality      uint32
}

// Payload fragments a point cloud packet across one or more byte arrays
func (p *PointCloudPayloader) Payload(mtu uint16, payload []byte) (payloads [][]byte) {
	payloadDataOffset := uint32(0)
	payloadLen := uint32(len(payload))
	payloadRemaining := payloadLen
	for payloadRemaining > 0 {
		currentFragmentSize := uint32(VideoPayloadSize)
		if payloadRemaining < currentFragmentSize {
			currentFragmentSize = payloadRemaining
		}
		buf := make([]byte, currentFragmentSize+VideoHeaderSize)
		binary.LittleEndian.PutUint32(buf[0:], uint32(*clientID))
		binary.LittleEndian.PutUint32(buf[4:], p.frameCounter)
		binary.LittleEndian.PutUint32(buf[8:], payloadLen)
		binary.LittleEndian.PutUint32(buf[12:], payloadDataOffset)
		binary.LittleEndian.PutUint32(buf[16:], currentFragmentSize)
		binary.LittleEndian.PutUint32(buf[20:], p.tile)
		binary.LittleEndian.PutUint32(buf[24:], p.quality)

		copy(buf[VideoHeaderSize:], payload[payloadDataOffset:(payloadDataOffset+currentFragmentSize)])

		payloads = append(payloads, buf)
		payloadDataOffset += currentFragmentSize
		payloadRemaining -= currentFragmentSize
	}
	p.frameCounter++
	return payloads
}

// New point cloud payloader
func NewPointCloudPayloader(tile uint32, quality uint32) *PointCloudPayloader {
	logger.Log(fmt.Sprintf("NewPointCloudPayloader started for tile %d and quality %d", tile, quality), LevelVerbose)
	return &PointCloudPayloader{0, tile, quality}
}

// Audio payloader
type AudioPayloader struct {
	frameCounter uint32
}

// Payload fragments an audio cloud packet across one or more byte arrays
func (p *AudioPayloader) Payload(mtu uint16, payload []byte) (payloads [][]byte) {
	payloadDataOffset := uint32(0)
	payloadLen := uint32(len(payload))
	payloadRemaining := payloadLen
	for payloadRemaining > 0 {
		currentFragmentSize := uint32(AudioPayloadSize)
		if payloadRemaining < currentFragmentSize {
			currentFragmentSize = payloadRemaining
		}
		buf := make([]byte, currentFragmentSize+AudioHeaderSize)
		binary.LittleEndian.PutUint32(buf[0:], uint32(*clientID))
		binary.LittleEndian.PutUint32(buf[4:], p.frameCounter)
		binary.LittleEndian.PutUint32(buf[8:], payloadLen)
		binary.LittleEndian.PutUint32(buf[12:], payloadDataOffset)
		binary.LittleEndian.PutUint32(buf[16:], currentFragmentSize)
		copy(buf[AudioHeaderSize:], payload[payloadDataOffset:(payloadDataOffset+currentFragmentSize)])
		payloads = append(payloads, buf)
		payloadDataOffset += currentFragmentSize
		payloadRemaining -= currentFragmentSize
	}
	p.frameCounter++
	return payloads
}

// New audio payloader
func NewAudioPayloader() *AudioPayloader {
	logger.Log("NewAudioPayloader started", LevelVerbose)
	return &AudioPayloader{}
}
