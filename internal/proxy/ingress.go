package proxy

// The callback direction.
//
// The tunnel could point straight at the application's port, and the callback
// would arrive. It points here instead, and this forwards, for two reasons the
// direct route cannot give:
//
//   - A RECEIPT. The egress side already answers "what did the sandbox
//     receive"; without something in the inbound path nothing can answer "what
//     did my app receive", and a webhook suite that passes because zero
//     webhooks arrived is the same silent success in a new direction.
//   - A BOUNDARY. Whatever the tunnel points at is reachable by anyone holding
//     the URL. Pointing it at a listener that forwards to exactly one declared
//     port means the proxy's own /__veris/status -- unauthenticated, and the
//     leak core-gaps already tracks -- can never be published by mistake.

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Callback is one inbound request that reached the app, keyed the way a client
// would assert on it.
type Callback struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Count  int64  `json:"count"`
}

// InboundReceipt is what arrived through the tunnel while the run was live.
type InboundReceipt struct {
	Total     int64      `json:"total"`
	Delivered int64      `json:"delivered"`
	Failed    int64      `json:"failed"`
	Callbacks []Callback `json:"callbacks"`
	// ByPath counts everything that arrived, including what never reached the
	// app; DeliveredByPath counts only what the app actually answered.
	// Assertions use the second: a callback the app never saw must not satisfy
	// a requirement that it did.
	ByPath          map[string]int64 `json:"by_path"`
	DeliveredByPath map[string]int64 `json:"delivered_by_path"`
}

type callbackKey struct {
	Method string
	Path   string
	Status int
}

// Ingress accepts what the tunnel forwards and passes it to the app.
type Ingress struct {
	// origin is the app, as a URL rather than a port, so the forwarder needs no
	// second idea of how to reach it.
	origin *url.URL
	proxy  *httputil.ReverseProxy

	total     atomic.Int64
	delivered atomic.Int64
	failed    atomic.Int64

	mu      sync.Mutex
	entries map[callbackKey]int64

	// What was already counted when the run began; subtracted from the receipt.
	base                                 map[callbackKey]int64
	baseTotal, baseDelivered, baseFailed int64
}

// DiscountOne excludes a single request from the run's receipt.
//
// For the confirmation probe, which travels this same path once the app is
// already listening. Baselining the whole receipt there would mark every
// callback that raced the probe as startup traffic and lose it, so exactly one
// request is discounted instead -- and never more, whatever else arrived.
func (in *Ingress) DiscountOne(method, path string, status int, delivered bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.baseTotal++
	// `failed` counts only transport failures, incremented by the reverse
	// proxy's error handler -- an app answering 4xx never touches it. So a
	// discount may reduce `delivered` and never `failed`, or the receipt
	// reports Failed: -1 for a probe the app merely rejected.
	if delivered {
		in.baseDelivered++
	}
	// The per-path entry as well as the aggregate. Leaving it behind lets a
	// path-specific --require-callback on the probe's own path -- "/", usually
	// -- pass with no real callback at all.
	if in.base == nil {
		in.base = map[callbackKey]int64{}
	}
	in.base[callbackKey{Method: method, Path: path, Status: status}]++
}

// RefundOne undoes a DiscountOne, for a probe that turned out never to have
// reached this listener.
func (in *Ingress) RefundOne(method, path string, status int, delivered bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.baseTotal > 0 {
		in.baseTotal--
	}
	if delivered && in.baseDelivered > 0 {
		in.baseDelivered--
	}
	k := callbackKey{Method: method, Path: path, Status: status}
	if in.base[k] > 0 {
		in.base[k]--
	}
}

// Baseline marks everything counted so far as not belonging to the run.
//
// Startup drives traffic through this same path -- the sandbox probes the URL
// to confirm it can reach the app -- and that probe is not a callback the run
// produced. Counting it would let `--require-callback '*'` be satisfied before
// the command under test had even started.
//
// A baseline rather than a reset, because by the time the confirmation probe
// runs the app is listening and may ALREADY have received real callbacks.
// Clearing would erase those, so the receipt would undercount exactly when it
// mattered most.
func (in *Ingress) Baseline() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.baseTotal = in.total.Load()
	in.baseDelivered = in.delivered.Load()
	in.baseFailed = in.failed.Load()
	in.base = make(map[callbackKey]int64, len(in.entries))
	for k, v := range in.entries {
		in.base[k] = v
	}
}

// DefaultOriginHost is where the app is, in the arrangement the proxy is
// normally run in.
//
// Loopback is not a simplification there: the workload container joins the
// proxy's network namespace, so the app's port is genuinely on our loopback
// with no networking in between -- measured, a sidecar workload is reachable at
// 127.0.0.1 from the proxy container. It is also why the workload can publish
// no port of its own, which is what makes a tunnel the natural answer here
// rather than a fallback.
//
// An app on the HOST while the proxy is in a container is the other case, and
// loopback is then the proxy container's own -- measured, connection refused.
// That case names host.docker.internal instead, which is the same escape hatch
// the API's docker backend already uses for callbacks.
const DefaultOriginHost = "127.0.0.1"

// NewIngress forwards to the app listening at host:port.
func NewIngress(host string, port int) (*Ingress, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("port %d is not a port to expose", port)
	}
	if host == "" {
		host = DefaultOriginHost
	}
	// JoinHostPort, so an IPv6 host becomes [::1]:8080 rather than ::1:8080,
	// which nothing can dial.
	origin, err := url.Parse("http://" + net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	in := &Ingress{origin: origin, entries: map[callbackKey]int64{}}

	in.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(origin)
			// The vendor's Host, not ours. A client verifying a signature over
			// the request -- which is most of why webhooks are hard -- must see
			// what the sandbox sent, and the tunnel edge has already rewritten
			// Host to its own name by the time it reaches us.
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			in.failed.Add(1)
			// 502 with the reason: the app not listening yet is the ordinary
			// case, and a silent failure here looks exactly like a vendor that
			// never sent anything.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = writeJSONBody(w, map[string]any{
				"error": "veris_app_unreachable",
				"message": fmt.Sprintf(
					"the callback reached the proxy but %s did not answer: %v",
					origin.Host, err),
			})
		},
		Transport: &http.Transport{
			// A local hop. These are short because a callback the app cannot
			// answer promptly is a failing callback, not a slow one.
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
	return in, nil
}

// Origin is where callbacks are forwarded, as host:port.
func (in *Ingress) Origin() string { return in.origin.Host }

// Handler serves the tunnel's forwarded requests.
func (in *Ingress) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.total.Add(1)
		// The recorder records the hit the moment the status is known, which is
		// BEFORE ServeHTTP finishes flushing the response. A workload that
		// exits the instant it replies could otherwise have its container seen
		// gone, and the receipt read, in the gap before a record-after-ServeHTTP
		// ran -- yielding an incomplete map and a false --require-callback fail.
		rec := &statusRecorder{
			ResponseWriter: w, status: http.StatusOK,
			onStatus: func(status int) { in.record(r.Method, r.URL.Path, status) },
		}
		in.proxy.ServeHTTP(rec, r)
		// A handler that wrote nothing at all still counts as a 200 delivered.
		rec.ensureRecorded()
	})
}

func (in *Ingress) record(method, path string, status int) {
	if status < 400 {
		in.delivered.Add(1)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.entries[callbackKey{Method: method, Path: path, Status: status}]++
}

// Receipt snapshots what arrived.
func (in *Ingress) Receipt() InboundReceipt {
	in.mu.Lock()
	defer in.mu.Unlock()

	out := InboundReceipt{
		// Clamped: the probe is discounted BEFORE it is issued, so a receipt
		// read while it is still in flight would otherwise report Total: -1.
		Total:           nonNegative(in.total.Load() - in.baseTotal),
		Delivered:       nonNegative(in.delivered.Load() - in.baseDelivered),
		Failed:          nonNegative(in.failed.Load() - in.baseFailed),
		Callbacks:       make([]Callback, 0, len(in.entries)),
		ByPath:          map[string]int64{},
		DeliveredByPath: map[string]int64{},
	}
	for k, n := range in.entries {
		if n -= in.base[k]; n <= 0 {
			continue
		}
		out.Callbacks = append(out.Callbacks, Callback{
			Method: k.Method, Path: k.Path, Status: k.Status, Count: n,
		})
		out.ByPath[k.Path] += n
		if k.Status < 400 {
			out.DeliveredByPath[k.Path] += n
		}
	}
	// Busiest first, then by name, so two runs diff readably.
	sort.Slice(out.Callbacks, func(i, j int) bool {
		if out.Callbacks[i].Count != out.Callbacks[j].Count {
			return out.Callbacks[i].Count > out.Callbacks[j].Count
		}
		if out.Callbacks[i].Path != out.Callbacks[j].Path {
			return out.Callbacks[i].Path < out.Callbacks[j].Path
		}
		return out.Callbacks[i].Method < out.Callbacks[j].Method
	})
	return out
}

func nonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// statusRecorder remembers the status so the receipt can separate a callback
// the app accepted from one it rejected. A 500 from the app is still a callback
// that ARRIVED, and conflating the two would let a broken handler read as a
// vendor that never sent anything.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	written  bool
	recorded bool
	onStatus func(int)
}

func (r *statusRecorder) WriteHeader(code int) {
	// 1xx is informational -- 103 Early Hints arrives BEFORE the real answer,
	// and ReverseProxy passes both through. Latching the first would report the
	// hint instead of whether the callback was accepted.
	if !r.written && code >= 200 {
		r.status = code
		r.written = true
		r.recordOnce()
	}
	r.ResponseWriter.WriteHeader(code)
}

// recordOnce reports the hit the first time a final status is known.
func (r *statusRecorder) recordOnce() {
	if r.recorded || r.onStatus == nil {
		return
	}
	r.recorded = true
	r.onStatus(r.status)
}

// ensureRecorded covers a handler that returned without ever writing a status.
func (r *statusRecorder) ensureRecorded() { r.recordOnce() }

// Unwrap exposes the real writer to http.ResponseController, which is how
// ReverseProxy hijacks a connection for a protocol upgrade. Without it a
// WebSocket the app accepted is turned into a 502 by the wrapper in front of it.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Write(b []byte) (int, error) {
	// An implicit 200: a body written with no explicit status.
	if !r.written {
		r.written = true
		r.recordOnce()
	}
	return r.ResponseWriter.Write(b)
}

// Flush keeps streamed responses streaming through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Attach publishes this ingress on the proxy's status endpoint, so a caller in
// another container can read what the app received. Without it `run --image`
// could enforce --require-service but not --require-callback.
func (s *Server) Attach(in *Ingress) { s.inbound.Store(&in) }

// AttachIngress is the same, from a started proxy.
func (r *Running) AttachIngress(in *Ingress) { r.srv.Attach(in) }
