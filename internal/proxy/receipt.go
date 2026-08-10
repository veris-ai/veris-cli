package proxy

import (
	"sort"
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
type receipt struct {
	mu      sync.Mutex
	entries map[receiptKey]int64
	total   int64
}

// receiptKey is host plus the routing decision, not host alone. Google fronts
// three services on www.googleapis.com, so a host-keyed receipt cannot tell a
// client that Calendar rather than Identity was exercised.
type receiptKey struct {
	Host    string
	Service string
	Prefix  string
}

// Hit is one host/service/prefix combination and how often it was sent.
type Hit struct {
	Host    string `json:"host"`
	Service string `json:"service"`
	Prefix  string `json:"prefix"`
	Count   int64  `json:"count"`
}

// Receipt is an immutable snapshot taken after the run.
type Receipt struct {
	Total     int64            `json:"total"`
	Hits      []Hit            `json:"hits"`
	ByHost    map[string]int64 `json:"by_host"`
	ByService map[string]int64 `json:"by_service"`
}

func (r *receipt) record(host string, target *config.Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[receiptKey]int64{}
	}
	r.entries[receiptKey{Host: host, Service: target.Service, Prefix: target.Prefix}]++
	r.total++
}

func (r *receipt) snapshot() Receipt {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := Receipt{
		Total:     r.total,
		Hits:      make([]Hit, 0, len(r.entries)),
		ByHost:    map[string]int64{},
		ByService: map[string]int64{},
	}
	for k, n := range r.entries {
		out.Hits = append(out.Hits, Hit{Host: k.Host, Service: k.Service, Prefix: k.Prefix, Count: n})
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
