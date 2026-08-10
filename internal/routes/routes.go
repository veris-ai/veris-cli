// Package routes holds the interception route table: which real hostname, and
// where a vendor shares one across products which path prefix, belongs to each
// simulated service.
//
// The table is GENERATED from each service's measured parity backend
// (`parity vendor-routes --write`) and embedded here, never hand-edited. A
// hand-maintained copy is a second place for the fact to live and a second
// chance to be wrong about it: one said Google served /tokeninfo from
// www.googleapis.com, while the measured record puts it on
// oauth2.googleapis.com.
package routes

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed vendor_routes.json
var tableJSON []byte

// Entry is one hostname a service answers on, optionally narrowed to the path
// prefixes measured on that host.
type Entry struct {
	Host  string   `json:"host"`
	Paths []string `json:"paths,omitempty"`
}

type table struct {
	Version  int                `json:"version"`
	Source   string             `json:"source"`
	Services map[string][]Entry `json:"services"`
}

var loaded table

func init() {
	if err := json.Unmarshal(tableJSON, &loaded); err != nil {
		// The file is embedded at build time and covered by a repository test,
		// so this is a build defect rather than a runtime condition.
		panic("veris-proxy: embedded vendor_routes.json is unreadable: " + err.Error())
	}
}

// For returns the hostnames a service answers on. The second result is false
// for a service with no measured vendor host -- one whose oracle is a locally
// run instance, which a client is not calling over the internet anyway.
func For(service string) ([]Entry, bool) {
	entries, ok := loaded.Services[service]
	return entries, ok && len(entries) > 0
}

// Known lists every service the table can route, sorted.
func Known() []string {
	names := make([]string, 0, len(loaded.Services))
	for name := range loaded.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Source describes where the table came from, for a message that has to
// explain why a service is missing from it.
func Source() string { return loaded.Source }

// Describe renders a service's routes for a human reading `use` output.
func Describe(service string) string {
	entries, ok := For(service)
	if !ok {
		return service + " (no vendor host measured)"
	}
	out := service
	for _, e := range entries {
		if len(e.Paths) == 0 {
			out += fmt.Sprintf("  %s", e.Host)
			continue
		}
		for _, p := range e.Paths {
			out += fmt.Sprintf("  %s%s", e.Host, p)
		}
	}
	return out
}
