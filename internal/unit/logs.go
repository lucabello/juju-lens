package unit

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

// logStreamer follows the service logs of workload containers through their
// Pebble sockets, with GET /v1/logs?follow=true, and queues the lines for the
// probe's main loop to write: the Pebble socket Juju shares with the charm
// container is all it needs, so no cluster access is involved.
type logStreamer struct {
	ctx  context.Context
	logf func(format string, args ...any)
	ch   chan PebbleLog

	mu      sync.Mutex
	started map[string]bool // by container
}

// logQueue is how many lines may wait for the main loop.
const logQueue = 1024

// logRetry is how long a stream waits before it reconnects.
const logRetry = 2 * time.Second

func newLogStreamer(ctx context.Context, logf func(string, ...any)) *logStreamer {
	return &logStreamer{ctx: ctx, logf: logf, ch: make(chan PebbleLog, logQueue), started: map[string]bool{}}
}

// add starts following container's logs through its Pebble socket, once.
func (ls *logStreamer) add(container, socket string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.started[container] {
		return
	}
	ls.started[container] = true
	go ls.follow(container, socket)
}

// follow streams one container's logs until the context ends. A stream that
// ends, because the container restarted, is reconnected: the lines of the
// gap that Pebble still buffers are read again, minus those already seen.
func (ls *logStreamer) follow(container, socket string) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	defer hc.CloseIdleConnections()
	cur := &logCursor{at: map[string]bool{}}
	first := true
	for ls.ctx.Err() == nil {
		// Only new lines at first, as the probe's other readings are of the
		// time it runs; after a drop, what the buffer still has.
		n := "0"
		if !first {
			n = "-1"
		}
		first = false
		err := ls.stream(hc, container, "http://pebble/v1/logs?follow=true&n="+n, cur)
		if err != nil && ls.ctx.Err() == nil {
			ls.logf("unit probe: logs of %s: %v; retrying", container, err)
		}
		select {
		case <-ls.ctx.Done():
			return
		case <-time.After(logRetry):
		}
	}
}

func (ls *logStreamer) stream(hc *http.Client, container, url string, cur *logCursor) error {
	req, err := http.NewRequestWithContext(ls.ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{resp.Status}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		l, ok := parsePebbleLog(sc.Bytes(), container)
		if !ok {
			continue
		}
		key := l.Service + "\x00" + l.Message
		switch {
		case l.Time.Before(cur.last), l.Time.Equal(cur.last) && cur.at[key]:
			continue // sent before the stream dropped
		case l.Time.After(cur.last):
			cur.last, cur.at = l.Time, map[string]bool{}
		}
		cur.at[key] = true
		select {
		case ls.ch <- l:
		case <-ls.ctx.Done():
			return nil
		}
	}
	return sc.Err()
}

// logCursor is how far a container's logs were sent: the time of the newest
// line, and the lines sent at that time, by service and message.
type logCursor struct {
	last time.Time
	at   map[string]bool
}

type httpStatusError struct{ status string }

func (e *httpStatusError) Error() string { return "pebble: " + e.status }

// parsePebbleLog reads one line of the logs endpoint's newline-delimited JSON.
func parsePebbleLog(line []byte, container string) (PebbleLog, bool) {
	var e struct {
		Time    time.Time `json:"time"`
		Service string    `json:"service"`
		Message string    `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Time.IsZero() {
		return PebbleLog{}, false
	}
	return PebbleLog{Container: container, Time: e.Time.UTC(), Service: e.Service, Message: e.Message}, true
}
