package api

import (
	"encoding/json"

	"github.com/veris-ai/veris-cli/internal/routes"
)

// Sandbox statuses, as models.py's SandboxStatus literal spells them.
// "failed" is terminal; "degraded" means some service is not ready yet and
// polling should continue; "terminating" is on its way out.
const (
	StatusProvisioning = "provisioning"
	StatusReady        = "ready"
	StatusDegraded     = "degraded"
	StatusFailed       = "failed"
	StatusTerminating  = "terminating"
)

// Service statuses (ServiceStatus).
const (
	ServicePending = "pending"
	ServiceReady   = "ready"
)

// Clock restore modes for promote and snapshot (ClockRestore). "rebase" is
// legacy and only accepted for baselines already promoted with it.
const (
	ClockToday  = "today"
	ClockFrozen = "frozen"
	ClockRebase = "rebase"
)

// EnvironmentBaseline is a promoted sandbox state pinned as what the
// environment boots; Image is a digest reference, never a tag.
type EnvironmentBaseline struct {
	Image         string `json:"image"`
	RevisionID    string `json:"revision_id"`
	PromotedAt    Time   `json:"promoted_at"`
	SourceSandbox string `json:"source_sandbox"`
}

// Environment is a named set of services; sandboxes are deployed from it.
type Environment struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Services  []string `json:"services"`
	CreatedAt Time     `json:"created_at"`
	Owner     string   `json:"owner"`
	// Baseline is nil when the environment boots the bundle image with
	// profile seeding.
	Baseline *EnvironmentBaseline `json:"baseline"`
}

// CreateEnvironmentRequest is POST /v1/environments' body.
type CreateEnvironmentRequest struct {
	Name     string   `json:"name,omitempty"`
	Services []string `json:"services"`
}

// ResetEnvironmentRequest is POST /v1/environments/{id}/reset's body: a nil
// BaselineImage is sent as null and clears the pin.
type ResetEnvironmentRequest struct {
	BaselineImage *string `json:"baseline_image"`
}

// ServiceInfo is one service of a sandbox as the control plane describes it.
type ServiceInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// URL is what the code under test points at: a gateway path for an http
	// service, a DSN for postgres.
	URL string `json:"url"`
	// ControlURL is where /veris/* lives, always an http gateway path.
	ControlURL string `json:"control_url"`
	EnvHint    string `json:"env_hint"`
	// Routes are the measured vendor hostnames this service answers for, and
	// the only source of them the proxy has. Null when the control plane has
	// none for it: the proxy then intercepts nothing for this service and
	// hands its URL to the command under EnvHint instead -- or, with no hint
	// to hand it under, reports it as out of reach.
	Routes []routes.Entry `json:"routes"`
}

// Sandbox is one running deployment of an environment.
type Sandbox struct {
	ID            string        `json:"id"`
	EnvironmentID string        `json:"environment_id"`
	Status        string        `json:"status"`
	CreatedAt     Time          `json:"created_at"`
	ExpiresAt     Time          `json:"expires_at"`
	Services      []ServiceInfo `json:"services"`
	// FailureReason is set when Status is "failed": why, in the container's
	// own words.
	FailureReason string `json:"failure_reason"`
	// Metadata echoes the create request's coordinates verbatim.
	Metadata map[string]string `json:"metadata"`
	// SnapshotID names the snapshot this sandbox booted from; empty for a
	// baseline or boot-profile sandbox.
	SnapshotID string `json:"snapshot_id"`
}

// CreateSandboxRequest is POST /v1/environments/{id}/sandboxes' body. Every
// field is optional; the zero value encodes as `{}`, meaning all defaults.
type CreateSandboxRequest struct {
	TTLMinutes    *int              `json:"ttl_minutes,omitempty"`
	SnapshotID    *string           `json:"snapshot_id,omitempty"`
	ClientBaseURL *string           `json:"client_base_url,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// SandboxUpdate is PATCH …/sandboxes/{id}'s body. The server reads which
// fields were SENT (model_fields_set): omitting client_base_url leaves it
// alone, sending null unregisters it. Clear sends the null; ClientBaseURL
// sends the value; neither sends `{}`.
type SandboxUpdate struct {
	ClientBaseURL *string
	Clear         bool
}

// MarshalJSON writes exactly the fields the update names.
func (u SandboxUpdate) MarshalJSON() ([]byte, error) {
	body := map[string]any{}
	switch {
	case u.Clear:
		body["client_base_url"] = nil
	case u.ClientBaseURL != nil:
		body["client_base_url"] = *u.ClientBaseURL
	}
	return json.Marshal(body)
}

// PromoteRequest is POST …/promote's body.
type PromoteRequest struct {
	ClockRestore             string `json:"clock_restore,omitempty"`
	KeepExternalDestinations bool   `json:"keep_external_destinations"`
}

// PromoteResponse is what a promote returns: the pinned baseline plus what
// the capture scrubbed, per service.
type PromoteResponse struct {
	EnvironmentID        string              `json:"environment_id"`
	SandboxID            string              `json:"sandbox_id"`
	Baseline             EnvironmentBaseline `json:"baseline"`
	ClockRestore         string              `json:"clock_restore"`
	SizeBytes            int64               `json:"size_bytes"`
	CuratorClockRestored bool                `json:"curator_clock_restored"`
	Scrubbed             map[string][]string `json:"scrubbed"`
}

// Snapshot is a captured sandbox world kept beside its environment
// (EnvironmentSnapshot).
type Snapshot struct {
	ID            string `json:"id"`
	EnvironmentID string `json:"environment_id"`
	Name          string `json:"name"`
	Image         string `json:"image"`
	RevisionID    string `json:"revision_id"`
	CreatedAt     Time   `json:"created_at"`
	SourceSandbox string `json:"source_sandbox"`
	ClockRestore  string `json:"clock_restore"`
	SizeBytes     int64  `json:"size_bytes"`
}

// CreateSnapshotRequest is POST /v1/environments/{id}/snapshots' body.
type CreateSnapshotRequest struct {
	SandboxID                string `json:"sandbox_id"`
	Name                     string `json:"name,omitempty"`
	ClockRestore             string `json:"clock_restore,omitempty"`
	KeepExternalDestinations bool   `json:"keep_external_destinations"`
}

// SnapshotResponse wraps a fresh snapshot with the same loudness a promote
// has about what the capture did to the curator.
type SnapshotResponse struct {
	Snapshot             Snapshot            `json:"snapshot"`
	CuratorClockRestored bool                `json:"curator_clock_restored"`
	Scrubbed             map[string][]string `json:"scrubbed"`
}

// ServiceResetResult is one service's outcome in a sandbox reset. Detail is
// the service's own reset response on success or the error string on
// failure, so it is kept raw.
type ServiceResetResult struct {
	Name   string          `json:"name"`
	OK     bool            `json:"ok"`
	Detail json.RawMessage `json:"detail"`
}

// ResetResponse is POST …/sandboxes/{id}/reset's answer.
type ResetResponse struct {
	ID       string               `json:"id"`
	OK       bool                 `json:"ok"`
	Services []ServiceResetResult `json:"services"`
}

// EgressCredential is what routes a sandbox's egress through the Veris
// gateway (BYOP gateway mode). CAPEM is the gateway's public certificate.
type EgressCredential struct {
	SocksAddress   string `json:"socks_address"`
	Username       string `json:"username"`
	CAPEM          string `json:"ca_pem"`
	CanaryHost     string `json:"canary_host"`
	ConnectAddress string `json:"connect_address"`
	HTTPProxyURL   string `json:"http_proxy_url"`
	MinSDK         string `json:"min_sdk"`
}

// GatewayHealth says whether the control plane offers gateway mode.
type GatewayHealth struct {
	Available  bool   `json:"available"`
	CanaryHost string `json:"canary_host"`
}

// CatalogService is one GET /v1/services entry (CatalogEntry).
type CatalogService struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	EnvHint     string         `json:"env_hint"`
	Routes      []routes.Entry `json:"routes"`
	// Requires names the services the platform adds to any sandbox holding
	// this one -- the issuer a product service signs in through. Naming one
	// service is asking for both, and the client's auth base URL resolves
	// without anyone having to know that.
	//
	// ProvidesFor is the other direction: for an issuer, the services that
	// bring it along.
	//
	// Both are empty from a control plane too old to serve them, which reads
	// as "no dependencies known" and must never read as an error.
	Requires    []string `json:"requires"`
	ProvidesFor []string `json:"provides_for"`
}

// Healthz is GET /healthz: unauthenticated, and Checkout fingerprints the
// source checkout the control plane was started from.
type Healthz struct {
	Status   string `json:"status"`
	Checkout string `json:"checkout"`
}

// APIKey is a key as the list shows it (ApiKeyInfo): identifiable, never
// recoverable. Status is "active" or "revoked".
type APIKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	Status    string `json:"status"`
	CreatedAt Time   `json:"created_at"`
	CreatedBy string `json:"created_by"`
	ExpiresAt Time   `json:"expires_at"`
}

// Me is GET /v1/me: who the presented credential is.
type Me struct {
	// Kind is "user" for a session, "api_key" for a key, "operator" for the
	// pre-tenancy secret.
	Kind string `json:"kind"`
	// User is nil for a key: a machine credential is not a person.
	User           *MeUser        `json:"user"`
	OrganizationID string         `json:"organization_id"`
	Organizations  []Organization `json:"organizations"`
	// Key describes the presented key -- the same row GET /v1/api-keys shows
	// for it -- on a control plane that sends it; older ones omit the field
	// and the CLI falls back to what it stored at login or the list route.
	Key *APIKey `json:"key,omitempty"`
}

// MeUser is the person behind a session.
type MeUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Organization is one of /v1/me's organizations rows (OrganizationInfo).
// Role is the caller's role in it; empty for a key, which has no membership.
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Kind string `json:"kind"`
	Role string `json:"role"`
}

// DeviceCodeRequest is POST /v1/device/code's body.
type DeviceCodeRequest struct {
	ClientName string `json:"client_name"`
}

// DeviceCodeResponse is a freshly minted pairing. DeviceCode is the polling
// secret and appears nowhere else.
type DeviceCodeResponse struct {
	UserCode                string `json:"user_code"`
	DeviceCode              string `json:"device_code"`
	VerificationURL         string `json:"verification_url"`
	VerificationURLComplete string `json:"verification_url_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceTokenRequest is POST /v1/device/token's body.
type DeviceTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

// DeviceTokenResponse is the redemption: the one answer that carries the
// minted key.
type DeviceTokenResponse struct {
	APIKey         string `json:"api_key"`
	OrganizationID string `json:"organization_id"`
	KeyID          string `json:"key_id"`
	KeyName        string `json:"key_name"`
}
