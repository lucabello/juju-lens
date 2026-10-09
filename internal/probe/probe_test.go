package probe

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/lucabello/juju-lens/internal/wire"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []Frame{
		{TsUnixNano: 111, PID: 42, Dir: "write", Conn: 9, Unit: "grafana/0", Data: []byte("hello")},
		{TsUnixNano: 222, PID: 42, Dir: "read", Conn: 9, Data: []byte{0x00, 0x01, 0x02}},
		{TsUnixNano: 333, PID: 42, Drops: 7}, // stats frame
	}
	for _, f := range in {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	for i, want := range in {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if got.TsUnixNano != want.TsUnixNano || got.PID != want.PID || got.Dir != want.Dir ||
			got.Conn != want.Conn || got.Unit != want.Unit || got.Drops != want.Drops ||
			!bytes.Equal(got.Data, want.Data) {
			t.Fatalf("frame %d roundtrip mismatch: got %+v want %+v", i, got, want)
		}
	}
}

// TestResolveSymbolsAgainstSelf resolves guaranteed-present function and method
// symbols in this test binary, proving the .gopclntab resolution path works on
// a real (PIE, stripped-of-.symtab) Go ELF. The crypto/tls specifics are
// deadcode-eliminated from a binary that never calls them, so those are
// validated against a real jujud rather than here — but the resolver itself,
// including method symbols like "os.(*File).Read", is exercised end-to-end.
func TestResolveSymbolsAgainstSelf(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot find test executable")
	}
	const method = "os.(*File).Read" // a method symbol, like the ones we really attach
	found, missing, err := ResolveSymbols(self, []string{"runtime.main", method, "no.such.Symbol"})
	if err != nil {
		t.Fatalf("ResolveSymbols: %v", err)
	}
	byName := map[string]uint64{}
	for _, s := range found {
		byName[s.Name] = s.Entry
	}
	if byName["runtime.main"] == 0 {
		t.Fatalf("expected runtime.main entry, got %+v", found)
	}
	if byName[method] == 0 {
		t.Fatalf("expected %s entry (method symbol resolution), got %+v", method, found)
	}
	if len(missing) != 1 || missing[0] != "no.such.Symbol" {
		t.Fatalf("expected the bogus symbol to be reported missing, got %v", missing)
	}
}

// TestResolveRETs disassembles a real function in this binary and confirms we
// find its RET instructions — the offsets the read path attaches uprobes to
// instead of using a (Go-unsafe) uretprobe.
func TestResolveRETs(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot find test executable")
	}
	rets, err := ResolveRETs(self, "runtime.main")
	if err != nil {
		t.Fatalf("ResolveRETs(runtime.main): %v", err)
	}
	if len(rets) == 0 {
		t.Fatal("expected at least one RET site in runtime.main")
	}
	for _, off := range rets {
		if off == 0 {
			t.Fatalf("RET offset should be a non-zero file offset, got %v", rets)
		}
	}
}

// TestIngestEndToEnd frames a full websocket request/response exchange the way
// the probe would, and checks Ingest reassembles it into paired
// CapturedMessages tagged with the right model and unit.
func TestIngestEndToEnd(t *testing.T) {
	var stream bytes.Buffer
	writeFrame := func(dir string, data []byte) {
		if err := WriteFrame(&stream, Frame{PID: 100, Dir: dir, Conn: 1, Unit: "grafana/0", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	// Handshake, then request (masked) and response (unmasked).
	writeFrame("write", []byte(wsGET))
	writeFrame("write", ws(t, `{"request-id":1,"type":"Uniter","request":"SetStatus","params":{}}`, true))
	writeFrame("read", []byte(ws101))
	writeFrame("read", ws(t, `{"request-id":1,"response":{}}`, false))

	var got []wire.CapturedMessage
	err := Ingest(context.Background(), &stream, nil, func(cm wire.CapturedMessage) error {
		got = append(got, cm)
		return nil
	}, nil, nil)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 captured messages, got %d: %+v", len(got), got)
	}
	if got[0].Model != "12345678-1234-1234-1234-1234567890ab" {
		t.Fatalf("model attribution wrong: %q", got[0].Model)
	}
	if got[0].Unit != "grafana/0" || !got[0].Msg.IsRequest() || got[1].Msg.RequestID != 1 {
		t.Fatalf("captured messages wrong: %+v", got)
	}
}

// TestIngestReportsAttachStatus checks onAttached fires for every
// attach-status frame, including a zero count, but not for drop-stats frames.
func TestIngestReportsAttachStatus(t *testing.T) {
	var stream bytes.Buffer
	for _, f := range []Frame{{Attached: 0}, {Drops: 3}, {Attached: 2}} {
		if err := WriteFrame(&stream, f); err != nil {
			t.Fatal(err)
		}
	}
	var got []int
	err := Ingest(context.Background(), &stream, nil, func(wire.CapturedMessage) error { return nil }, nil, func(n int) { got = append(got, n) })
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("onAttached calls = %v, want [0 2]", got)
	}
}

// ws builds a single masked/unmasked text frame carrying payload.
func ws(t *testing.T, payload string, mask bool) []byte {
	t.Helper()
	p := []byte(payload)
	var b []byte
	b = append(b, 0x81) // FIN + text
	maskBit := byte(0)
	if mask {
		maskBit = 0x80
	}
	if len(p) >= 126 {
		t.Fatalf("test payload too long")
	}
	b = append(b, maskBit|byte(len(p)))
	if mask {
		key := []byte{1, 2, 3, 4}
		b = append(b, key...)
		for i := range p {
			b = append(b, p[i]^key[i&3])
		}
	} else {
		b = append(b, p...)
	}
	return b
}

const wsGET = "GET /model/12345678-1234-1234-1234-1234567890ab/api HTTP/1.1\r\n\r\n"
const ws101 = "HTTP/1.1 101 Switching Protocols\r\n\r\n"
