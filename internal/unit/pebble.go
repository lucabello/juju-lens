package unit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path"
	"time"
)

// pebbleClient reads from a workload container through its Pebble socket,
// which Juju shares with the charm container under
// /charm/containers/<name>/pebble.socket. Pebble runs as the container's
// PID 1, so the files it serves are the workload container's own: its
// /sys/fs/cgroup is the workload's cgroup.
type pebbleClient struct {
	hc     *http.Client
	socket string
}

func newPebbleClient(socket string) *pebbleClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}
	return &pebbleClient{hc: &http.Client{Transport: tr, Timeout: 2 * time.Second}, socket: socket}
}

// readFiles pulls the named files of dir in one GET /v1/files?action=read.
// It is a filesReader. Pebble answers with a multipart body: a "files" part
// per file it could read, named by its path, and a "response" part with
// the JSON result, which holds an error for each file it couldn't.
func (p *pebbleClient) readFiles(dir string, names []string) (map[string][]byte, error) {
	q := url.Values{"action": {"read"}}
	for _, n := range names {
		q.Add("path", path.Join(dir, n))
	}
	req, err := http.NewRequest(http.MethodGet, "http://pebble/v1/files?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "multipart/form-data")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseFilesResponse(resp.Header.Get("Content-Type"), resp.Body)
}

// parseFilesResponse returns the files of a read response, by base name,
// and the first error it reports.
func parseFilesResponse(contentType string, body io.Reader) (map[string][]byte, error) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "multipart/form-data" {
		return nil, pebbleError(body)
	}
	mr := multipart.NewReader(body, params["boundary"])
	files := map[string][]byte{}
	var first error
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, err
		}
		switch part.FormName() {
		case "files":
			content, err := io.ReadAll(io.LimitReader(part, 1<<20))
			if err != nil {
				return files, err
			}
			// FileName is the base name of the path: the name in dir.
			files[part.FileName()] = content
		case "response":
			var r struct {
				Result []struct {
					Error *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"result"`
			}
			if json.NewDecoder(part).Decode(&r) == nil {
				for _, res := range r.Result {
					if res.Error != nil && first == nil {
						first = errors.New("pebble: " + res.Error.Message)
					}
				}
			}
		}
	}
	return files, first
}

func pebbleError(body io.Reader) error {
	var r struct {
		Result struct {
			Message string `json:"message"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&r); err == nil && r.Result.Message != "" {
		return errors.New("pebble: " + r.Result.Message)
	}
	return errors.New("pebble: unexpected response")
}

// changes lists every change Pebble still remembers.
func (p *pebbleClient) changes() ([]PebbleChange, error) {
	resp, err := p.hc.Get("http://pebble/v1/changes?select=all")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pebble: changes: %s", resp.Status)
	}
	return parseChanges(resp.Body)
}

func parseChanges(body io.Reader) ([]PebbleChange, error) {
	var r struct {
		Result []struct {
			ID        string    `json:"id"`
			Kind      string    `json:"kind"`
			Summary   string    `json:"summary"`
			Status    string    `json:"status"`
			Ready     bool      `json:"ready"`
			Err       string    `json:"err"`
			SpawnTime time.Time `json:"spawn-time"`
			ReadyTime time.Time `json:"ready-time"`
		} `json:"result"`
	}
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return nil, err
	}
	out := make([]PebbleChange, 0, len(r.Result))
	for _, c := range r.Result {
		out = append(out, PebbleChange{
			ID: c.ID, Kind: c.Kind, Summary: c.Summary, Status: c.Status,
			Ready: c.Ready, Err: c.Err, Spawn: c.SpawnTime.UTC(), ReadyTime: c.ReadyTime.UTC(),
		})
	}
	return out, nil
}
