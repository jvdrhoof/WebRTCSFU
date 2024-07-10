package main

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// NoOp is an Interceptor that does not modify any packets. It can embedded in other interceptors, so it's
// possible to implement only a subset of the methods.

// ResponderInterceptorFactory is a interceptor.Factory for a ResponderInterceptor
type TrackMergerInterceptorFactory struct {
}

// NewInterceptor constructs a new ResponderInterceptor
func (r *TrackMergerInterceptorFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	i := &TrackMerger{}
	return i, nil
}

type TrackMerger struct {
}

// NewResponderInterceptor returns a new ResponderInterceptorFactor
func NewTrackMergerInterceptor() (*TrackMergerInterceptorFactory, error) {
	return &TrackMergerInterceptorFactory{}, nil
}

// BindRTCPReader lets you modify any incoming RTCP packets. It is called once per sender/receiver, however this might
// change in the future. The returned method will be called once per packet batch.
func (i *TrackMerger) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return reader
}

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (i *TrackMerger) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return writer
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (i *TrackMerger) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	seqNr := uint16(0)
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		header.SequenceNumber = seqNr
		seqNr++
		return writer.Write(header, payload, attributes)
	})
}

// UnbindLocalStream is called when the Stream is removed. It can be used to clean up any data related to that track.
func (i *TrackMerger) UnbindLocalStream(_ *interceptor.StreamInfo) {}

// BindRemoteStream lets you modify any incoming RTP packets. It is called once for per RemoteStream. The returned method
// will be called once per rtp packet.
func (i *TrackMerger) BindRemoteStream(_ *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	return reader
}

// UnbindRemoteStream is called when the Stream is removed. It can be used to clean up any data related to that track.
func (i *TrackMerger) UnbindRemoteStream(_ *interceptor.StreamInfo) {}

// Close closes the Interceptor, cleaning up any data if necessary.
func (i *TrackMerger) Close() error {
	return nil
}
