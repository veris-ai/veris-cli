// Package routes holds the shape of an interception route: which real
// hostname, and where a vendor shares one across products which path prefix,
// belongs to a simulated service.
//
// The routes themselves live in one place only -- the control plane, which
// serves them with every sandbox and from GET /v1/services. They are
// GENERATED there from each service's measured vendor backend, never
// authored. This binary keeps no copy: a second copy is a second place for
// the fact to live and a second chance to be wrong about it, and a stale one
// that routed silently would be worse than no copy at all. A service the
// control plane serves no hostname for is not intercepted; --route supplies
// one for a single run.
package routes

// Entry is one hostname a service answers on, optionally narrowed to the path
// prefixes measured on that host.
type Entry struct {
	Host  string   `json:"host"`
	Paths []string `json:"paths,omitempty"`
}
