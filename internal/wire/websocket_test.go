package wire

import (
	"encoding/binary"
	"testing"
)

// buildFrame constructs a single RFC 6455 data frame. When mask is true the
// payload is masked (as a client→server frame is).
func buildFrame(payload []byte, fin, mask bool, opcode byte) []byte {
	var b []byte
	first := opcode
	if fin {
		first |= 0x80
	}
	b = append(b, first)
	n := len(payload)
	var maskBit byte
	if mask {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		b = append(b, maskBit|byte(n))
	case n < 1<<16:
		b = append(b, maskBit|126)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(n))
		b = append(b, l[:]...)
	default:
		b = append(b, maskBit|127)
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		b = append(b, l[:]...)
	}
	if mask {
		key := []byte{0xde, 0xad, 0xbe, 0xef}
		b = append(b, key...)
		masked := make([]byte, n)
		for i := 0; i < n; i++ {
			masked[i] = payload[i] ^ key[i&3]
		}
		b = append(b, masked...)
	} else {
		b = append(b, payload...)
	}
	return b
}

const wsGET = "GET /model/12345678-1234-1234-1234-1234567890ab/api HTTP/1.1\r\n" +
	"Host: controller:17070\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

const ws101 = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

func TestConnDemuxHandshakeAndFrame(t *testing.T) {
	d := NewConnDemux(1)
	// Write side: handshake then a masked request frame.
	if got := d.Feed(DirWrite, []byte(wsGET)); len(got) != 0 {
		t.Fatalf("handshake alone should yield no envelopes, got %d", len(got))
	}
	reqJSON := []byte(`{"request-id":5,"type":"Uniter","request":"SetStatus","params":{}}`)
	env := d.Feed(DirWrite, buildFrame(reqJSON, true, true, 0x1))
	if len(env) != 1 {
		t.Fatalf("expected 1 request envelope, got %d", len(env))
	}
	if env[0].Type != "Uniter" || env[0].Request != "SetStatus" || env[0].RequestID != 5 {
		t.Fatalf("request decoded wrong: %+v", env[0])
	}
	if d.Model() != "12345678-1234-1234-1234-1234567890ab" {
		t.Fatalf("model UUID not extracted from path, got %q", d.Model())
	}

	// Read side: handshake then an unmasked response frame.
	d.Feed(DirRead, []byte(ws101))
	respJSON := []byte(`{"request-id":5,"response":{"ok":true}}`)
	env = d.Feed(DirRead, buildFrame(respJSON, true, false, 0x1))
	if len(env) != 1 || env[0].RequestID != 5 || env[0].IsRequest() {
		t.Fatalf("response decoded wrong: %+v", env)
	}
}

func TestConnDemuxFragmentedAndSplit(t *testing.T) {
	d := NewConnDemux(2)
	d.Feed(DirRead, []byte(ws101))

	payload := []byte(`{"request-id":9,"response":{"big":"value"}}`)
	// Two continuation frames: first (opcode text, fin=false), second (opcode
	// continuation 0x0, fin=true).
	half := len(payload) / 2
	f1 := buildFrame(payload[:half], false, false, 0x1)
	f2 := buildFrame(payload[half:], true, false, 0x0)
	all := append(f1, f2...)

	// Deliver the whole thing one byte at a time to prove reassembly across
	// arbitrary chunk boundaries.
	var got []Envelope
	for _, bb := range all {
		got = append(got, d.Feed(DirRead, []byte{bb})...)
	}
	if len(got) != 1 || got[0].RequestID != 9 {
		t.Fatalf("fragmented+split message not reassembled: %+v", got)
	}
}

func TestConnDemuxControlFramesIgnored(t *testing.T) {
	d := NewConnDemux(3)
	d.Feed(DirRead, []byte(ws101))
	// A ping (opcode 0x9) carries no application data.
	if got := d.Feed(DirRead, buildFrame([]byte("hi"), true, false, 0x9)); len(got) != 0 {
		t.Fatalf("control frame should yield no envelopes, got %+v", got)
	}
	env := d.Feed(DirRead, buildFrame([]byte(`{"request-id":1}`), true, false, 0x1))
	if len(env) != 1 {
		t.Fatalf("data frame after control frame lost: %+v", env)
	}
}

func TestConnDemuxMidStreamAttach(t *testing.T) {
	// Attaching to an already-running agent means we never see the handshake:
	// the first bytes on each direction are websocket frames, not GET/HTTP.
	d := NewConnDemux(9)
	// A masked client request frame arrives first on the write side.
	env := d.Feed(DirWrite, buildFrame([]byte(`{"request-id":7,"type":"Uniter","request":"CommitHookChanges","params":{}}`), true, true, 0x1))
	if len(env) != 1 || env[0].Request != "CommitHookChanges" {
		t.Fatalf("mid-stream write frame not parsed without handshake: %+v", env)
	}
	// And an unmasked server response frame first on the read side.
	env = d.Feed(DirRead, buildFrame([]byte(`{"request-id":7,"response":{}}`), true, false, 0x1))
	if len(env) != 1 || env[0].RequestID != 7 || env[0].IsRequest() {
		t.Fatalf("mid-stream read frame not parsed without handshake: %+v", env)
	}
	if d.Model() != "" {
		t.Fatalf("mid-stream connection cannot know its model, got %q", d.Model())
	}
}

func TestConnDemuxControllerConnHasNoModel(t *testing.T) {
	d := NewConnDemux(4)
	get := "GET /api HTTP/1.1\r\nHost: c\r\n\r\n"
	d.Feed(DirWrite, []byte(get))
	if d.Model() != "" {
		t.Fatalf("controller-wide /api connection should have no model, got %q", d.Model())
	}
}
