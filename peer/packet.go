package main

type FramePacket struct {
	FrameNr   uint32
	TileNr    uint32
	TileLen   uint32
	SeqOffset uint32
	SeqLen    uint32
	Data      [1148]byte
}

func NewFramePacket(frameNr, tileNr, tileLen, seqOffset, seqLen uint32, dataSubArray []byte) *FramePacket {
	packet := &FramePacket{
		FrameNr:   frameNr,
		TileNr:    tileNr,
		TileLen:   tileLen,
		SeqOffset: seqOffset,
		SeqLen:    seqLen,
	}
	copy(packet.Data[:], dataSubArray[seqOffset:(seqOffset+seqLen)])
	return packet
}
