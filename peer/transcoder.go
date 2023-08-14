package main

import (
	"bytes"
	"encoding/binary"
)

type Frame struct {
	ClientID uint32
	FrameLen uint32
	FrameNr  uint32
	Data     []byte
}

type Transcoder interface {
	UpdateBitrate(bitrate uint32)
	UpdateProjection()
	EncodeFrame(uint32) *Frame
	IsReady() bool
}

type TranscoderFiles struct {
	frames       []FileData
	frameCounter uint32
	isReady      bool
	fileCounter  uint32
}

func (f *Frame) Bytes() []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, f.ClientID)
	binary.Write(buf, binary.BigEndian, f.FrameLen)
	binary.Write(buf, binary.BigEndian, f.FrameNr)
	binary.Write(buf, binary.BigEndian, f.Data)
	return buf.Bytes()
}

func NewTranscoderFile(contentDirectory string) *TranscoderFiles {
	fBytes, _ := ReadBinaryFiles(contentDirectory)
	return &TranscoderFiles{fBytes, 0, true, 0}
}

func (t *TranscoderFiles) UpdateBitrate(bitrate uint32) {
	// Do nothing
}

func (t *TranscoderFiles) UpdateProjection() {
	// Do nothing
}

func (t *TranscoderFiles) EncodeFrame(tile uint32) *Frame {
	rFrame := Frame{0, uint32(len(t.frames[t.fileCounter].Data)), t.frameCounter, t.frames[t.fileCounter].Data}
	t.frameCounter = (t.frameCounter + 1)
	t.fileCounter = (t.fileCounter + 1) % uint32(len(t.frames))
	return &rFrame
}

func (t *TranscoderFiles) IsReady() bool {
	return t.isReady
}

type TranscoderRemote struct {
	proxy_con    *ProxyConnection
	frameCounter uint32
	isReady      bool
}

func NewTranscoderRemote(proxy_con *ProxyConnection) *TranscoderRemote {
	return &TranscoderRemote{proxy_con, 0, true}
}

func (t *TranscoderRemote) UpdateBitrate(bitrate uint32) {
	// Do nothing
}

func (t *TranscoderRemote) UpdateProjection() {
	// Do nothing
}

func (t *TranscoderRemote) EncodeFrame(tile uint32) *Frame {
	// println("Waiting for content")
	data := proxyConn.NextTile(tile)
	// println("Content arrived")
	rFrame := Frame{0, uint32(len(data)), t.frameCounter, data}
	t.frameCounter++
	return &rFrame
}

func (t *TranscoderRemote) IsReady() bool {
	return t.isReady
}

type TranscoderDummy struct {
	proxy_con    *ProxyConnection
	frameCounter uint32
	isReady      bool
}

func NewTranscoderDummy(proxy_con *ProxyConnection) *TranscoderDummy {
	return &TranscoderDummy{proxy_con, 0, true}
}

func (t *TranscoderDummy) UpdateBitrate(bitrate uint32) {
	// Do nothing
}

func (t *TranscoderDummy) UpdateProjection() {
	// Do nothing
}

func (t *TranscoderDummy) EncodeFrame(tile uint32) *Frame {
	return nil
}

func (t *TranscoderDummy) IsReady() bool {
	return true
}
