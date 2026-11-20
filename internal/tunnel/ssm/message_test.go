package ssm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"regexp"
	"testing"
	"time"
)

// layout builds a message byte by byte from the documented offsets, so the
// test does not trust marshal to check marshal.
func layout(typ string, created uint64, seq int64, flags uint64, id uuid, pt payloadType, payload []byte) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint32(b, 116)
	b = append(b, typ...)
	b = append(b, bytes.Repeat([]byte{' '}, 32-len(typ))...)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = binary.BigEndian.AppendUint64(b, created)
	b = binary.BigEndian.AppendUint64(b, uint64(seq))
	b = binary.BigEndian.AppendUint64(b, flags)
	b = append(b, id[8:]...) // least significant half first
	b = append(b, id[:8]...)
	digest := sha256.Sum256(payload)
	b = append(b, digest[:]...)
	b = binary.BigEndian.AppendUint32(b, uint32(pt))
	b = binary.BigEndian.AppendUint32(b, uint32(len(payload)))
	return append(b, payload...)
}

var testID = uuid{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}

func TestMarshalLayout(t *testing.T) {
	m := message{
		Type:          typeInput,
		SchemaVersion: 1,
		CreatedDate:   0x0102030405060708,
		Sequence:      7,
		Flags:         0,
		ID:            testID,
		PayloadType:   payloadOutput,
		Payload:       []byte("hi"),
	}
	got := m.marshal()
	want := layout(typeInput, 0x0102030405060708, 7, 0, testID, payloadOutput, []byte("hi"))
	if !bytes.Equal(got, want) {
		t.Fatalf("marshal() =\n%x\nwant\n%x", got, want)
	}
	if len(got) != 122 {
		t.Errorf("len(marshal()) = %d, want 122", len(got))
	}
	// The wire carries the UUID 08..0f then 00..07.
	if wire := got[64:80]; !bytes.Equal(wire, []byte{8, 9, 10, 11, 12, 13, 14, 15, 0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Errorf("message ID bytes = %x", wire)
	}
}

func TestParseMessage(t *testing.T) {
	good := layout(typeOutput, 1700000000000, 3, 1, testID, payloadOutput, []byte("payload"))
	m, err := parseMessage(good)
	if err != nil {
		t.Fatalf("parseMessage() error = %v", err)
	}
	want := message{Type: typeOutput, SchemaVersion: 1, CreatedDate: 1700000000000, Sequence: 3, Flags: 1,
		ID: testID, PayloadType: payloadOutput, Payload: []byte("payload")}
	if m.Type != want.Type || m.SchemaVersion != want.SchemaVersion || m.CreatedDate != want.CreatedDate ||
		m.Sequence != want.Sequence || m.Flags != want.Flags || m.ID != want.ID ||
		m.PayloadType != want.PayloadType || !bytes.Equal(m.Payload, want.Payload) {
		t.Errorf("parseMessage() = %+v, want %+v", m, want)
	}

	nulPadded := bytes.Clone(good)
	copy(nulPadded[4+len(typeOutput):36], make([]byte, 32-len(typeOutput)))
	if m, err := parseMessage(nulPadded); err != nil || m.Type != typeOutput {
		t.Errorf("parseMessage(NUL padded type) = %q, %v, want %q", m.Type, err, typeOutput)
	}

	// A longer header moves the payload; the fixed fields stay put.
	longer := bytes.Clone(good[:120])
	binary.BigEndian.PutUint32(longer, 120)
	longer = append(longer, 0, 0, 0, 0)
	longer = append(longer, good[120:]...)
	if m, err := parseMessage(longer); err != nil || string(m.Payload) != "payload" {
		t.Errorf("parseMessage(header length 120) payload = %q, %v, want %q", m.Payload, err, "payload")
	}

	badDigest := bytes.Clone(good)
	badDigest[len(badDigest)-1] ^= 0xff
	publication := layout(typeStartPublication, 0, 0, 0, testID, 0, nil)
	binary.BigEndian.PutUint32(publication[116:], 5) // a wrong length is not checked either
	empty := layout(typeAcknowledge, 1, 0, 3, testID, 0, nil)
	tooLong := bytes.Clone(good)
	binary.BigEndian.PutUint32(tooLong, uint32(len(good)))
	tooShort := bytes.Clone(good)
	binary.BigEndian.PutUint32(tooShort, 100)
	noType := layout("", 1, 0, 0, testID, 0, nil)

	tests := []struct {
		name    string
		in      []byte
		wantErr bool
	}{
		{"bad digest", badDigest, true},
		{"publication notices skip the digest", publication, false},
		{"empty payload", empty, false},
		{"truncated", good[:119], true},
		{"header length past the end", tooLong, true},
		{"header length inside the header", tooShort, true},
		{"no type", noType, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMessage(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMessage() error = %v, want error %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, errBadMessage) {
				t.Errorf("parseMessage() error = %v, want errBadMessage", err)
			}
		})
	}
}

func TestMessageRoundTrip(t *testing.T) {
	m := newMessage(typeAcknowledge, 0, flagsAck, 0, []byte(`{"a":1}`), time.UnixMilli(1234))
	got, err := parseMessage(m.marshal())
	if err != nil {
		t.Fatalf("parseMessage(marshal()) error = %v", err)
	}
	if got.Type != m.Type || got.ID != m.ID || got.CreatedDate != 1234 || got.Flags != flagsAck || string(got.Payload) != `{"a":1}` {
		t.Errorf("parseMessage(marshal()) = %+v, want %+v", got, m)
	}
}

func TestUUID(t *testing.T) {
	if got, want := testID.String(), "00010203-0405-0607-0809-0a0b0c0d0e0f"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, b := newUUID(), newUUID()
	if !v4.MatchString(a.String()) {
		t.Errorf("newUUID() = %s, want a version 4 UUID", a)
	}
	if a == b {
		t.Errorf("newUUID() returned %s twice", a)
	}
}
