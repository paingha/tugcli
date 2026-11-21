package ssm

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Message types of the Session Manager data channel.
const (
	typeInput            = "input_stream_data"
	typeOutput           = "output_stream_data"
	typeAcknowledge      = "acknowledge"
	typeChannelClosed    = "channel_closed"
	typeStartPublication = "start_publication"
	typePausePublication = "pause_publication"
)

// payloadType says what the payload of a stream data message carries.
type payloadType uint32

// Payload types used by port sessions.
const (
	payloadOutput               payloadType = 1
	payloadHandshakeRequest     payloadType = 5
	payloadHandshakeResponse    payloadType = 6
	payloadHandshakeComplete    payloadType = 7
	payloadEncChallengeRequest  payloadType = 8
	payloadEncChallengeResponse payloadType = 9
	payloadFlag                 payloadType = 10
	payloadStdErr               payloadType = 11
	payloadExitCode             payloadType = 12
)

// flag is the payload of a payloadFlag message, sent as a big-endian uint32.
type flag uint32

// Flags of port sessions.
const (
	flagTerminateSession   flag = 2
	flagConnectToPortError flag = 3
)

// The binary message is a fixed header followed by the payload. Integers are
// big-endian. The header length field counts everything before the payload
// length field, so the payload starts four bytes after it.
//
//	offset  size  field
//	     0     4  header length (116)
//	     4    32  message type, padded with spaces
//	    36     4  schema version
//	    40     8  created date, milliseconds since the Unix epoch
//	    48     8  sequence number
//	    56     8  flags
//	    64    16  message ID: the UUID's low eight bytes, then its high eight
//	    80    32  SHA-256 digest of the payload
//	   112     4  payload type
//	   116     4  payload length
//	   120     n  payload
const (
	offType          = 4
	offSchemaVersion = 36
	offCreatedDate   = 40
	offSequence      = 48
	offFlags         = 56
	offID            = 64
	offDigest        = 80
	offPayloadType   = 112
	offPayloadLength = 116
	offPayload       = 120

	typeLength   = offSchemaVersion - offType
	headerLength = offPayloadLength
)

// Flag bits of the header.
const (
	flagsNone = 0
	// flagsAck marks a message that stands alone, such as an
	// acknowledgement: it both starts (SYN) and ends (FIN) its stream.
	flagsAck = 3
)

// schemaVersion is the version of the binary message format.
const schemaVersion = 1

// message is one binary message of the data channel.
type message struct {
	Type          string
	SchemaVersion uint32
	CreatedDate   uint64
	Sequence      int64
	Flags         uint64
	ID            uuid
	PayloadType   payloadType
	Payload       []byte
}

// newMessage returns a message of the given type created now with a fresh ID.
func newMessage(typ string, seq int64, flags uint64, pt payloadType, payload []byte, now time.Time) message {
	return message{
		Type:          typ,
		SchemaVersion: schemaVersion,
		CreatedDate:   uint64(now.UnixMilli()),
		Sequence:      seq,
		Flags:         flags,
		ID:            newUUID(),
		PayloadType:   pt,
		Payload:       payload,
	}
}

// marshal encodes the message in the binary format. The type must fit in 32
// bytes, as every message type does.
func (m *message) marshal() []byte {
	b := make([]byte, offPayload+len(m.Payload))
	binary.BigEndian.PutUint32(b, headerLength)
	copy(b[offType:offSchemaVersion], bytes.Repeat([]byte{' '}, typeLength))
	copy(b[offType:offSchemaVersion], m.Type)
	binary.BigEndian.PutUint32(b[offSchemaVersion:], m.SchemaVersion)
	binary.BigEndian.PutUint64(b[offCreatedDate:], m.CreatedDate)
	binary.BigEndian.PutUint64(b[offSequence:], uint64(m.Sequence))
	binary.BigEndian.PutUint64(b[offFlags:], m.Flags)
	copy(b[offID:], m.ID[8:])
	copy(b[offID+8:], m.ID[:8])
	digest := sha256.Sum256(m.Payload)
	copy(b[offDigest:], digest[:])
	binary.BigEndian.PutUint32(b[offPayloadType:], uint32(m.PayloadType))
	binary.BigEndian.PutUint32(b[offPayloadLength:], uint32(len(m.Payload)))
	copy(b[offPayload:], m.Payload)
	return b
}

// errBadMessage marks a message that cannot be decoded or fails its digest.
var errBadMessage = errors.New("malformed data channel message")

// parseMessage decodes a binary message. The payload aliases b.
func parseMessage(b []byte) (message, error) {
	if len(b) < offPayload {
		return message{}, fmt.Errorf("%w: %d bytes is shorter than the header", errBadMessage, len(b))
	}
	hl := binary.BigEndian.Uint32(b)
	if hl < headerLength || uint64(hl)+4 > uint64(len(b)) {
		return message{}, fmt.Errorf("%w: header length %d does not fit %d bytes", errBadMessage, hl, len(b))
	}
	m := message{
		Type:          string(bytes.TrimSpace(bytes.Trim(b[offType:offSchemaVersion], "\x00"))),
		SchemaVersion: binary.BigEndian.Uint32(b[offSchemaVersion:]),
		CreatedDate:   binary.BigEndian.Uint64(b[offCreatedDate:]),
		Sequence:      int64(binary.BigEndian.Uint64(b[offSequence:])),
		Flags:         binary.BigEndian.Uint64(b[offFlags:]),
		PayloadType:   payloadType(binary.BigEndian.Uint32(b[offPayloadType:])),
		Payload:       b[hl+4:],
	}
	copy(m.ID[8:], b[offID:offID+8])
	copy(m.ID[:8], b[offID+8:offDigest])
	if m.Type == "" {
		return message{}, fmt.Errorf("%w: no message type", errBadMessage)
	}
	// The plugin skips the digest of publication notices, which carry no
	// payload worth checking.
	if m.Type == typeStartPublication || m.Type == typePausePublication {
		return m, nil
	}
	if binary.BigEndian.Uint32(b[offPayloadLength:]) != 0 {
		digest := sha256.Sum256(m.Payload)
		if !bytes.Equal(digest[:], b[offDigest:offPayloadType]) {
			return message{}, fmt.Errorf("%w: %s %d has a wrong payload digest", errBadMessage, m.Type, m.Sequence)
		}
	}
	return m, nil
}

// uuid is a UUID in the byte order of its string form.
type uuid [16]byte

// newUUID returns a random (version 4) UUID.
func newUUID() uuid {
	var u uuid
	// crypto/rand.Read never fails; it crashes the program if the system
	// cannot supply randomness.
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

// String formats the UUID as lowercase hex in 8-4-4-4-12 groups, the form
// acknowledgements quote.
func (u uuid) String() string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:], u[10:])
	return string(b[:])
}

// openDataChannel is the first message on the WebSocket, sent as text. It
// hands over the token from StartSession.
type openDataChannel struct {
	MessageSchemaVersion string `json:"MessageSchemaVersion"`
	RequestID            string `json:"RequestId"`
	TokenValue           string `json:"TokenValue"`
	ClientID             string `json:"ClientId"`
	ClientVersion        string `json:"ClientVersion"`
}

// ackContent is the payload of an acknowledge message.
type ackContent struct {
	MessageType string `json:"AcknowledgedMessageType"`
	MessageID   string `json:"AcknowledgedMessageId"`
	Sequence    int64  `json:"AcknowledgedMessageSequenceNumber"`
	Sequential  bool   `json:"IsSequentialMessage"`
}

// channelClosed is the payload of a channel_closed message.
type channelClosed struct {
	MessageID     string `json:"MessageId"`
	CreatedDate   string `json:"CreatedDate"`
	DestinationID string `json:"DestinationId"`
	SessionID     string `json:"SessionId"`
	MessageType   string `json:"MessageType"`
	SchemaVersion int    `json:"SchemaVersion"`
	Output        string `json:"Output"`
}

// Handshake actions the agent can request.
const (
	actionSessionType   = "SessionType"
	actionKMSEncryption = "KMSEncryption"
)

// actionStatus reports how the client handled a requested action.
type actionStatus int

// Action statuses.
const (
	actionSuccess     actionStatus = 1
	actionFailed      actionStatus = 2
	actionUnsupported actionStatus = 3
)

// sessionTypePort is the session type of port forwarding documents.
const sessionTypePort = "Port"

// handshakeRequest is the agent's first stream message.
type handshakeRequest struct {
	AgentVersion           string            `json:"AgentVersion"`
	RequestedClientActions []requestedAction `json:"RequestedClientActions"`
}

// requestedAction is one thing the agent asks the client to set up.
type requestedAction struct {
	ActionType       string          `json:"ActionType"`
	ActionParameters json.RawMessage `json:"ActionParameters"`
}

// sessionTypeRequest is the parameter of the SessionType action.
type sessionTypeRequest struct {
	SessionType string          `json:"SessionType"`
	Properties  json.RawMessage `json:"Properties"`
}

// handshakeResponse is the client's answer to the handshake request.
type handshakeResponse struct {
	ClientVersion          string            `json:"ClientVersion"`
	ProcessedClientActions []processedAction `json:"ProcessedClientActions"`
	Errors                 []string          `json:"Errors"`
}

// processedAction reports the outcome of one requested action.
type processedAction struct {
	ActionType   string       `json:"ActionType"`
	ActionStatus actionStatus `json:"ActionStatus"`
	ActionResult any          `json:"ActionResult"`
	Error        string       `json:"Error"`
}

// handshakeComplete tells the client the session has started.
type handshakeComplete struct {
	HandshakeTimeToComplete time.Duration `json:"HandshakeTimeToComplete"`
	CustomerMessage         string        `json:"CustomerMessage"`
}
