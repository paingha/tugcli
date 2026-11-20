package iap

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The relay protocol, from the Cloud SDK's iap_tunnel_websocket_utils.py.
// Every WebSocket message is one binary frame: a big-endian two-byte tag,
// then for CONNECT_SUCCESS_SID and DATA a four-byte length and that many
// bytes, and for RECONNECT_SUCCESS_ACK and ACK an eight-byte count of
// stream bytes.
const (
	// subprotocol is the WebSocket subprotocol the relay speaks.
	subprotocol = "relay.tunnel.cloudproxy.app"

	// tagConnectSuccessSID opens a new session and carries its ID.
	tagConnectSuccessSID uint16 = 0x0001
	// tagReconnectSuccessAck resumes a session and carries the count of
	// stream bytes the relay has received.
	tagReconnectSuccessAck uint16 = 0x0002
	// tagData carries stream bytes.
	tagData uint16 = 0x0004
	// tagAck carries the count of stream bytes received so far.
	tagAck uint16 = 0x0007

	tagLen    = 2
	lengthLen = 4
	countLen  = 8

	// maxDataFrame is the most stream bytes one DATA frame carries.
	maxDataFrame = 16384
)

// errProtocol marks frames the relay should not have sent. They end the
// tunnel rather than resume it.
var errProtocol = errors.New("the IAP relay broke the tunnel protocol")

// frame is one decoded relay frame.
type frame struct {
	tag   uint16
	data  []byte // DATA's bytes, or CONNECT_SUCCESS_SID's session ID
	count uint64 // ACK's or RECONNECT_SUCCESS_ACK's count of bytes
	extra int    // bytes after the frame in the same message
}

// tagName names a tag for messages.
func tagName(tag uint16) string {
	switch tag {
	case tagConnectSuccessSID:
		return "CONNECT_SUCCESS_SID"
	case tagReconnectSuccessAck:
		return "RECONNECT_SUCCESS_ACK"
	case tagData:
		return "DATA"
	case tagAck:
		return "ACK"
	default:
		return fmt.Sprintf("tag %#04x", tag)
	}
}

// appendData appends a DATA frame carrying p, at most maxDataFrame bytes.
func appendData(b, p []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, tagData)
	b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
	return append(b, p...)
}

// appendAck appends an ACK frame for n stream bytes received.
func appendAck(b []byte, n uint64) []byte {
	b = binary.BigEndian.AppendUint16(b, tagAck)
	return binary.BigEndian.AppendUint64(b, n)
}

// parseFrame decodes the frame at the start of msg. A frame with a tag it
// does not know comes back with only the tag, for the caller to skip, as
// gcloud does. The data aliases msg.
func parseFrame(msg []byte) (frame, error) {
	if len(msg) < tagLen {
		return frame{}, fmt.Errorf("%w: a message of %d bytes has no frame tag", errProtocol, len(msg))
	}
	f := frame{tag: binary.BigEndian.Uint16(msg)}
	rest := msg[tagLen:]
	switch f.tag {
	case tagConnectSuccessSID, tagData:
		if len(rest) < lengthLen {
			return frame{}, fmt.Errorf("%w: %s frame without its length", errProtocol, tagName(f.tag))
		}
		n := uint64(binary.BigEndian.Uint32(rest))
		rest = rest[lengthLen:]
		if uint64(len(rest)) < n {
			return frame{}, fmt.Errorf("%w: %s frame of %d bytes says it has %d", errProtocol, tagName(f.tag), len(rest), n)
		}
		f.data, rest = rest[:n], rest[n:]
	case tagReconnectSuccessAck, tagAck:
		if len(rest) < countLen {
			return frame{}, fmt.Errorf("%w: %s frame without its byte count", errProtocol, tagName(f.tag))
		}
		f.count, rest = binary.BigEndian.Uint64(rest), rest[countLen:]
	default:
		return f, nil
	}
	f.extra = len(rest)
	return f, nil
}
