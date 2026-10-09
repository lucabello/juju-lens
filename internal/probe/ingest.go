package probe

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/lucabello/juju-lens/internal/wire"
)

// NameResolver maps the UUIDs the probe derives from an agent's agent.conf to
// human names (via the juju client). Either returned name may be empty when
// unknown, in which case the UUID is used verbatim.
type NameResolver func(controllerUUID, modelUUID string) (controllerName, modelName string)

// Ingest reads probe frames from r, reassembles the websocket RPC stream per
// connection, and delivers a fully-decoded wire.CapturedMessage to sink for
// every RPC envelope that survives the core-signal filter. It returns when r
// reaches EOF, ctx is cancelled, or sink returns an error.
//
// resolve enriches each message's topology with controller/model names; it may
// be nil, in which case UUIDs are used verbatim.
//
// onDrops, if non-nil, is called with the cumulative ring-buffer drop count
// reported by the probe so the recorder can record gap intervals.
//
// onAttached, if non-nil, is called with the probe's attached-process count
// on every attach-status frame, including ones reporting 0. Every frame read
// after a call with n>0 was captured with the uprobes in place.
func Ingest(
	ctx context.Context,
	r io.Reader,
	resolve NameResolver,
	sink func(wire.CapturedMessage) error,
	onDrops func(total uint64),
	onAttached func(n int),
) error {
	demuxes := map[connKey]*wire.ConnDemux{}
	// suppressed remembers, per connection, the request-ids of RPCs we dropped
	// as noise so we can drop their responses too (a response carries no facade,
	// only a request-id, so it can only be classified by its request).
	suppressed := map[connKey]map[uint64]bool{}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, err := ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if f.Drops > 0 && onDrops != nil {
			onDrops(f.Drops)
		}
		if len(f.Data) == 0 && f.Drops == 0 && onAttached != nil {
			onAttached(f.Attached)
		}
		if len(f.Data) == 0 {
			continue // pure stats frame
		}
		dir := wire.DirRead
		if f.Dir == string(wire.DirWrite) {
			dir = wire.DirWrite
		}
		key := connKey{pid: f.PID, conn: f.Conn}
		dx := demuxes[key]
		if dx == nil {
			dx = wire.NewConnDemux(f.Conn)
			demuxes[key] = dx
		}
		envelopes := dx.Feed(dir, f.Data)
		if len(envelopes) == 0 {
			continue
		}
		// The probe reports UUIDs (from agent.conf); resolve them to names.
		// Fall back to the model UUID from the (rarely seen) handshake.
		modelUUID := f.Model
		if modelUUID == "" {
			modelUUID = dx.Model()
		}
		ctrlName, modelName := f.Controller, modelUUID
		if resolve != nil {
			if cn, mn := resolve(f.Controller, modelUUID); true {
				if cn != "" {
					ctrlName = cn
				}
				if mn != "" {
					modelName = mn
				}
			}
		}
		if modelName == "" {
			modelName = "controller"
		}
		ts := time.Unix(0, f.TsUnixNano).UTC()
		for _, e := range envelopes {
			if filtered(key, e, suppressed) {
				continue
			}
			cm := wire.CapturedMessage{
				Ts:         ts,
				PID:        f.PID,
				Dir:        dir,
				Conn:       f.Conn,
				Controller: ctrlName,
				Model:      modelName,
				ModelUUID:  modelUUID,
				App:        f.App,
				Unit:       f.Unit,
				Msg:        e,
			}
			if err := sink(cm); err != nil {
				return err
			}
		}
	}
}

// filtered reports whether an envelope should be dropped. We keep only the
// "core signal" RPCs (see keepRPC); a request that isn't core is dropped and its
// request-id recorded so the matching response — which carries no facade, only
// the id — is dropped too. Keeping this in the ingest path means recordings
// store only useful RPCs while the live --plain/--decode views still show
// everything.
func filtered(key connKey, e wire.Envelope, suppressed map[connKey]map[uint64]bool) bool {
	if e.IsRequest() {
		if keepRPC(e.Type, e.Request) {
			return false
		}
		s := suppressed[key]
		if s == nil {
			s = map[uint64]bool{}
			suppressed[key] = s
		}
		s[e.RequestID] = true
		return true
	}
	// Response: drop it iff we dropped its request.
	if s := suppressed[key]; s != nil && s[e.RequestID] {
		delete(s, e.RequestID)
		return true
	}
	return false
}

// coreMethods is the allowlist of high-value RPCs the recording keeps: the ones
// that carry unit/app state, hook lifecycle, relation data, leadership, secrets,
// config and ports. Everything else (keepalives, environmental constants,
// address lookups, polling) is dropped. Tune this as the analysis needs grow.
var coreMethods = map[string]map[string]bool{
	"Uniter": {
		"CommitHookChanges":              true, // bundled hook writes
		"SetState":                       true, // uniter lifecycle/op/hook
		"SetUnitStatus":                  true, // workload status (active/blocked/…)
		"SetAgentStatus":                 true, // agent status (executing/idle/…)
		"SetApplicationStatus":           true,
		"SetStatus":                      true, // legacy status alias
		"ReadSettings":                   true, // relation databags
		"ReadLocalApplicationSettings":   true,
		"ReadRemoteSettings":             true,
		"ReadLocalApplicationSettingsV2": true,
		"ConfigSettings":                 true,
		"OpenedPortRangesByEndpoint":     true,
		"EnterScope":                     true, // relation membership
		"LeaveScope":                     true,
	},
	"LeadershipService": {
		"ClaimLeadership": true,
	},
	"SecretsManager": {
		"GetSecretMetadata":    true,
		"GetSecretContentInfo": true,
		"CreateSecretURIs":     true,
		"UpdateSecrets":        true,
	},
}

// keepRPC reports whether a facade/method pair is core signal worth recording.
func keepRPC(facade, method string) bool {
	if m := coreMethods[facade]; m != nil {
		return m[method]
	}
	return false
}

type connKey struct {
	pid  int
	conn uint64
}
