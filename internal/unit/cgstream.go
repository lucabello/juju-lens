package unit

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// A workload container's cgroup files are read through Pebble, and Pebble
// logs every file its file API serves: a security event and a request line,
// in the workload's own logs, which COS ships to Loki. Reading them 10 times
// a second that way would flood those logs. So the probe starts one shell in
// each workload container with Pebble's exec API, for the whole recording,
// and asks it for a reading by writing a line to its stdin: Pebble logs the
// exec once, and nothing per reading. The shell reads the files with its
// builtins, so a reading starts no process. It exits when its stdin closes,
// which is when the probe stops or goes away. A container with no sh falls
// back to the file API, at the snapshot rate only.

// cgroupScript is the shell the stream runs: for each line on stdin, it
// prints every readable file of cgroupFiles as "== <name>" and its lines,
// then "==".
var cgroupScript = `cd /sys/fs/cgroup || exit 1
while read -r _; do
  s=
  for f in ` + strings.Join(cgroupFiles, " ") + `; do
    [ -r "$f" ] || continue
    s="$s== $f
"
    while IFS= read -r l || [ -n "$l" ]; do s="$s$l
"; done < "$f"
  done
  printf '%s==\n' "$s"
done`

// streamTimeout bounds one reading through the stream.
const streamTimeout = 2 * time.Second

// cgroupStream is a running cgroup shell in a workload container.
type cgroupStream struct {
	ctl, io *wsConn
	buf     []byte
}

// startCgroupStream starts the cgroup shell in p's container. The exec's
// change goes into own as soon as there is one, even if the shell then
// fails.
func startCgroupStream(p *pebbleClient, own map[string]bool) (*cgroupStream, error) {
	changeID, taskID, err := p.exec([]string{"sh", "-c", cgroupScript})
	if changeID != "" {
		own[changeID] = true
	}
	if err != nil {
		return nil, err
	}
	s := &cgroupStream{}
	// Pebble starts the command once both websockets are connected.
	if s.ctl, err = p.websocket(taskID, "control"); err != nil {
		return nil, err
	}
	if s.io, err = p.websocket(taskID, "stdio"); err != nil {
		s.ctl.close()
		return nil, err
	}
	// A shell that can't run (no sh in the image) fails its first reading.
	if _, err := s.readFiles("", nil); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// readFiles is a filesReader: it asks the shell for a reading and parses
// it. The shell always reads cgroupFiles in /sys/fs/cgroup; dir and names
// are only there to fit the type.
func (s *cgroupStream) readFiles(string, []string) (map[string][]byte, error) {
	deadline := time.Now().Add(streamTimeout)
	if err := s.io.write(opBinary, []byte("\n"), deadline); err != nil {
		return nil, err
	}
	s.buf = s.buf[:0]
	for !bytes.HasSuffix(s.buf, []byte("\n==\n")) && !bytes.Equal(s.buf, []byte("==\n")) {
		op, data, err := s.io.read(deadline)
		if err != nil {
			return nil, err
		}
		switch op {
		case opBinary, opCont:
			s.buf = append(s.buf, data...)
		case opText:
			// {"command":"end"}: the shell's output ended.
			return nil, fmt.Errorf("pebble: cgroup shell ended: %s", truncate(strings.TrimSpace(string(s.buf)), 200))
		}
	}
	return parseCgroupReading(s.buf), nil
}

// parseCgroupReading splits one reading of the cgroup shell into files.
func parseCgroupReading(b []byte) map[string][]byte {
	files := map[string][]byte{}
	var name string
	var content []byte
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("==")); ok {
			if name != "" {
				files[name] = content
			}
			name, content = strings.TrimSpace(string(rest)), nil
			continue
		}
		content = append(content, line...)
	}
	return files
}

func (s *cgroupStream) close() {
	if s.io != nil {
		// Ends the shell's stdin, so it exits; closing the websocket
		// alone does as well, when the probe dies.
		_ = s.io.write(opText, []byte(`{"command":"end"}`), time.Now().Add(time.Second))
		s.io.close()
	}
	if s.ctl != nil {
		s.ctl.close()
	}
}

// exec starts a command with POST /v1/exec and returns its change and task.
func (p *pebbleClient) exec(command []string) (changeID, taskID string, err error) {
	body, _ := json.Marshal(map[string]any{"command": command})
	resp, err := p.hc.Post("http://pebble/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var r struct {
		Change string `json:"change"`
		Result struct {
			TaskID  string `json:"task-id"`
			Message string `json:"message"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r); err != nil {
		return "", "", fmt.Errorf("pebble: exec: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted || r.Result.TaskID == "" {
		return "", "", fmt.Errorf("pebble: exec: %s %s", resp.Status, r.Result.Message)
	}
	return r.Change, r.Result.TaskID, nil
}

// websocket opens one of an exec task's websockets.
func (p *pebbleClient) websocket(taskID, name string) (*wsConn, error) {
	conn, err := net.DialTimeout("unix", p.socket, streamTimeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(streamTimeout))
	var key [16]byte
	_, _ = rand.Read(key[:])
	fmt.Fprintf(conn, "GET /v1/tasks/%s/websocket/%s HTTP/1.1\r\nHost: pebble\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		taskID, name, base64.StdEncoding.EncodeToString(key[:]))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		err := pebbleError(resp.Body)
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("websocket %s: %s: %w", name, resp.Status, err)
	}
	return &wsConn{conn: conn, br: br}, nil
}

// wsConn is the client end of a websocket: just what the cgroup stream
// needs (RFC 6455).
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xa
)

// write sends one message. A client masks what it sends.
func (w *wsConn) write(op byte, payload []byte, deadline time.Time) error {
	_ = w.conn.SetWriteDeadline(deadline)
	frame := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xffff:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := w.conn.Write(frame)
	return err
}

// read returns the next data frame, answering pings on the way.
func (w *wsConn) read(deadline time.Time) (op byte, payload []byte, err error) {
	_ = w.conn.SetReadDeadline(deadline)
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return 0, nil, err
		}
		op = h[0] & 0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var l [2]byte
			if _, err := io.ReadFull(w.br, l[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(l[:]))
		case 127:
			var l [8]byte
			if _, err := io.ReadFull(w.br, l[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(l[:])
		}
		if n > 1<<20 {
			return 0, nil, errors.New("websocket: frame too large")
		}
		var mask [4]byte
		masked := h[1]&0x80 != 0
		if masked {
			if _, err := io.ReadFull(w.br, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		payload = make([]byte, n)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case opPing:
			if err := w.write(opPong, payload, deadline); err != nil {
				return 0, nil, err
			}
		case opPong:
		case opClose:
			return 0, nil, io.EOF
		default:
			return op, payload, nil
		}
	}
}

// close ends the connection with a close frame.
func (w *wsConn) close() {
	_ = w.write(opClose, nil, time.Now().Add(time.Second))
	_ = w.conn.Close()
}
