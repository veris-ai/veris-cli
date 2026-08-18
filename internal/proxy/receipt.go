package proxy

import (
	"sort"
	"strings"
	"sync"

	"github.com/veris-ai/veris-proxy/internal/config"
)

// receipt records what a run actually sent to the sandbox.
//
// It answers a different question from the canary token. The canary proves
// interception was live before the command started; the receipt proves the
// command reached the sandbox while it ran. A suite that silently stopped
// calling its dependency passes the canary check and produces an empty
// receipt, which is the failure worth catching.
//
// Counts are attempts, not logical operations: a retried request is two hits.
//
// Control-plane requests -- /veris/* seeding, manuals, wire traces -- are
// counted apart from vendor-surface traffic. They are usually the HARNESS
// talking to the sandbox, not the code under test, and folding them together
// let a run whose SDK calls all failed TLS satisfy --require-service with its
// own setup reads.
type receipt struct {
	mu      sync.Mutex
	entries map[receiptKey]int64
	total   int64
	control int64
}

// receiptKey is host plus the routing decision, not host alone. Google fronts
// three services on www.googleapis.com, so a host-keyed receipt cannot tell a
// client that Calendar rather than Identity was exercised.
type receiptKey struct {
	Host    string
	Service string
	Prefix  string
	Control bool
}

// Hit is one host/service/prefix combination and how often it was sent.
// Control marks /veris/* control-plane requests, which the ByHost/ByService
// aggregates exclude.
type Hit struct {
	Host    string `json:"host"`
	Service string `json:"service"`
	Prefix  string `json:"prefix"`
	Control bool   `json:"control,omitempty"`
	Count   int64  `json:"count"`
}

// Receipt is an immutable snapshot taken after the run. Total, ByHost and
// ByService count vendor-surface traffic only -- the requests the code under
// test sent to what it believes is the vendor. Control-plane requests appear
// in Hits with Control set and in the ByServiceControl/ControlTotal
// aggregates, so a run's verdict can never be satisfied by its own harness.
type Receipt struct {
	Total     int64            `json:"total"`
	Hits      []Hit            `json:"hits"`
	ByHost    map[string]int64 `json:"by_host"`
	ByService map[string]int64 `json:"by_service"`

	// ControlTotal counts /veris/* control-plane requests, kept out of Total.
	ControlTotal int64 `json:"control_total,omitempty"`
	// ByServiceControl is ControlTotal per service, for the printed receipt
	// and for requirement messages that explain what was NOT counted.
	ByServiceControl map[string]int64 `json:"by_service_control,omitempty"`

	// TrustFailures lists, per SNI host, TLS handshakes that ended after the
	// proxy selected a certificate: the client saw the minted leaf and
	// refused or abandoned it. Entries here beside zero completed requests
	// are what an SDK-bundled CA bundle looks like from outside.
	TrustFailures []TrustFailure `json:"tls_trust_failures,omitempty"`
}

// IsControlPlanePath reports whether path belongs to the sandbox's /veris/*
// control plane. The trailing slash (or exact match) is load-bearing: a bare
// prefix check would also claim vendor paths that merely start with the same
// six letters (/verisign-callback, /version).
func IsControlPlanePath(path string) bool {
	return path == "/veris" || strings.HasPrefix(path, "/veris/")
}

func (r *receipt) record(host string, target *config.Target, control bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[receiptKey]int64{}
	}
	r.entries[receiptKey{Host: host, Service: target.Service, Prefix: target.Prefix, Control: control}]++
	if control {
		r.control++
		return
	}
	r.total++
}

func (r *receipt) snapshot() Receipt {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := Receipt{
		Total:        r.total,
		ControlTotal: r.control,
		Hits:         make([]Hit, 0, len(r.entries)),
		ByHost:       map[string]int64{},
		ByService:    map[string]int64{},
	}
	for k, n := range r.entries {
		out.Hits = append(out.Hits, Hit{
			Host: k.Host, Service: k.Service, Prefix: k.Prefix,
			Control: k.Control, Count: n,
		})
		if k.Control {
			if out.ByServiceControl == nil {
				out.ByServiceControl = map[string]int64{}
			}
			out.ByServiceControl[k.Service] += n
			continue
		}
		out.ByHost[k.Host] += n
		out.ByService[k.Service] += n
	}
	// Busiest first, then by name, so the printed summary is stable across runs
	// and a diff of two runs is readable.
	sort.Slice(out.Hits, func(i, j int) bool {
		if out.Hits[i].Count != out.Hits[j].Count {
			return out.Hits[i].Count > out.Hits[j].Count
		}
		if out.Hits[i].Service != out.Hits[j].Service {
			return out.Hits[i].Service < out.Hits[j].Service
		}
		return out.Hits[i].Prefix < out.Hits[j].Prefix
	})
	return out
}
