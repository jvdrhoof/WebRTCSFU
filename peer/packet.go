package main

const WebRTCPayloadSize = 1172

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
