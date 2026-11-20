package iap

import (
	"bytes"
	"errors"
	"testing"
)

func TestAppendFrames(t *testing.T) {
	if got, want := appendData(nil, []byte("hi")), []byte{0x00, 0x04, 0, 0, 0, 2, 'h', 'i'}; !bytes.Equal(got, want) {
		t.Errorf("appendData(hi) = % x, want % x", got, want)
	}
	if got, want := appendAck(nil, 0x0102030405060708), []byte{0x00, 0x07, 1, 2, 3, 4, 5, 6, 7, 8}; !bytes.Equal(got, want) {
		t.Errorf("appendAck() = % x, want % x", got, want)
	}
}

func TestParseFrame(t *testing.T) {
	tests := []struct {
		name string
		msg  []byte
		want frame
	}{
		{
			name: "connect success",
			msg:  []byte{0x00, 0x01, 0, 0, 0, 3, 's', 'i', 'd'},
			want: frame{tag: tagConnectSuccessSID, data: []byte("sid")},
		},
		{
			name: "reconnect success",
			msg:  []byte{0x00, 0x02, 0, 0, 0, 0, 0, 0, 0x40, 0x00},
			want: frame{tag: tagReconnectSuccessAck, count: 16384},
		},
		{
			name: "data",
			msg:  []byte{0x00, 0x04, 0, 0, 0, 2, 'o', 'k'},
			want: frame{tag: tagData, data: []byte("ok")},
		},
		{
			name: "empty data",
			msg:  []byte{0x00, 0x04, 0, 0, 0, 0},
			want: frame{tag: tagData, data: []byte{}},
		},
		{
			name: "ack",
			msg:  []byte{0x00, 0x07, 0, 0, 0, 0, 0, 0, 0, 9},
			want: frame{tag: tagAck, count: 9},
		},
		{
			name: "extra bytes",
			msg:  []byte{0x00, 0x07, 0, 0, 0, 0, 0, 0, 0, 9, 0xff, 0xff},
			want: frame{tag: tagAck, count: 9, extra: 2},
		},
		{
			name: "unknown tag",
			msg:  []byte{0x00, 0x09, 1, 2, 3},
			want: frame{tag: 0x0009},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFrame(tt.msg)
			if err != nil {
				t.Fatalf("parseFrame(% x): %v", tt.msg, err)
			}
			if got.tag != tt.want.tag || !bytes.Equal(got.data, tt.want.data) || got.count != tt.want.count || got.extra != tt.want.extra {
				t.Errorf("parseFrame(% x) = %+v, want %+v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestParseFrameRejectsShortFrames(t *testing.T) {
	for _, msg := range [][]byte{
		{},
		{0x00},
		{0x00, 0x01, 0, 0},
		{0x00, 0x04, 0, 0, 0, 5, 'a'},
		{0x00, 0x02, 0, 0, 0},
		{0x00, 0x07, 0, 0, 0, 0, 0, 0, 0},
	} {
		if _, err := parseFrame(msg); !errors.Is(err, errProtocol) {
			t.Errorf("parseFrame(% x) error = %v, want errProtocol", msg, err)
		}
	}
}

func TestDataRoundTrip(t *testing.T) {
	p := bytes.Repeat([]byte{0xa5}, maxDataFrame)
	f, err := parseFrame(appendData(nil, p))
	if err != nil || f.tag != tagData || !bytes.Equal(f.data, p) || f.extra != 0 {
		t.Errorf("parseFrame(appendData(%d bytes)) = %s of %d bytes, %v; want DATA of %d bytes", len(p), tagName(f.tag), len(f.data), err, len(p))
	}
}
