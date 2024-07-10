package main

type FramePacketHeader struct {
	ClientNr  uint32 // 4
	FrameNr   uint32 // 8
	FrameLen  uint32 // 12
	SeqOffset uint32 // 16
	SeqLen    uint32 // 20
}

type VideoFramePacket struct {
	FramePacketHeader        // 20
	TileNr            uint32 // 24
	Data              [1148]byte
}

type AudioFramePacket struct {
	FramePacketHeader // 20
	Data              [1152]byte
}
