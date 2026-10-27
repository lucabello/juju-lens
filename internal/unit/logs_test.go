package unit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestLogStreamerReconnectsWithoutRepeating: the stream follows new lines
// only at first; after it drops it asks for what Pebble still buffers and
// leaves out the lines it already sent.
func TestLogStreamerReconnectsWithoutRepeating(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "pebble.socket")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	line := func(i int, msg string) string {
		return fmt.Sprintf(`{"time":%q,"service":"svc","message":%q}`+"\n", t0.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), msg)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("n"))
		first := len(asked) == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		if first {
			fmt.Fprint(w, line(0, "one"))
			return // drops the stream
		}
		// A reconnect: the buffer has the old line, a new one, and a second
		// line at the same time as the new one.
		fmt.Fprint(w, line(0, "one"), line(1, "two"), line(1, "three"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})}
	go srv.Serve(ln)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ls := newLogStreamer(ctx, t.Logf)
	ls.add("app", sock)
	ls.add("app", sock) // once

	var got []string
	for len(got) < 3 {
		select {
		case l := <-ls.ch:
			if l.Container != "app" || l.Service != "svc" {
				t.Errorf("line %+v", l)
			}
			got = append(got, l.Message)
		case <-time.After(10 * time.Second):
			t.Fatalf("got %v, want one, two, three", got)
		}
	}
	if fmt.Sprint(got) != "[one two three]" {
		t.Errorf("lines %v, want each once in order", got)
	}
	select {
	case l := <-ls.ch:
		t.Errorf("repeated %+v", l)
	case <-time.After(200 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 2 || asked[0] != "0" || asked[1] != "-1" {
		t.Errorf("asked for n=%v: new lines first, then the buffer", asked)
	}
}
