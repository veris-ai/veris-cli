package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/routes"
)

// Fixtures shaped like api/tests' answers: a sandbox as test_sandboxes.py
// reads one back, an egress credential as test_egress_credential.py checks
// it, the /v1/me shapes from auth_login.py, a promote from test_promote.py.
const (
	sandboxID = "k3j2v0d8p1q7x9r2m5n8b4c6a"
	envID     = "e1v2i3r4o5n6m7e8n9t0a1b2c"
	snapID    = "s1n2a3p4s5h6o7t8i9d0a1b2c"

	sandboxJSON = `{
	  "id": "k3j2v0d8p1q7x9r2m5n8b4c6a",
	  "environment_id": "e1v2i3r4o5n6m7e8n9t0a1b2c",
	  "status": "provisioning",
	  "created_at": "2026-09-03T12:00:00+00:00",
	  "expires_at": "2026-09-03T14:00:00Z",
	  "services": [
	    {"name": "stripe", "status": "pending",
	     "url": "http://203.0.113.10/s/k3j2v0d8p1q7x9r2m5n8b4c6a/stripe",
	     "control_url": "http://203.0.113.10/s/k3j2v0d8p1q7x9r2m5n8b4c6a/stripe",
	     "env_hint": "STRIPE_API_BASE",
	     "routes": [{"host": "api.stripe.com", "paths": null}]},
	    {"name": "postgres", "status": "ready",
	     "url": "postgresql://k3j2v0d8p1q7x9r2m5n8b4c6a:x@pg.test:5432/k3j2v0d8p1q7x9r2m5n8b4c6a",
	     "control_url": "http://203.0.113.10/s/k3j2v0d8p1q7x9r2m5n8b4c6a/postgres",
	     "env_hint": "DATABASE_URL", "routes": null}
	  ],
	  "failure_reason": null,
	  "metadata": {"run": "billing-rl-07", "epoch": "3"},
	  "snapshot_id": null
	}`

	environmentJSON = `{
	  "id": "e1v2i3r4o5n6m7e8n9t0a1b2c", "name": "billing",
	  "services": ["stripe", "postgres"],
	  "created_at": "2026-09-01T08:15:30.123456+00:00",
	  "owner": "org_1",
	  "baseline": {"image": "reg.example/env-repo/env-e1v2i3r4o5n6m7e8n9t0a1b2c@sha256:abc",
	               "revision_id": "rev_7", "promoted_at": "2026-09-02T00:00:00Z",
	               "source_sandbox": "k3j2v0d8p1q7x9r2m5n8b4c6a"}
	}`

	snapshotJSON = `{
	  "id": "s1n2a3p4s5h6o7t8i9d0a1b2c", "environment_id": "e1v2i3r4o5n6m7e8n9t0a1b2c",
	  "name": "before-run-3",
	  "image": "reg.example/env-repo/env-e1v2i3r4o5n6m7e8n9t0a1b2c@sha256:def",
	  "revision_id": "rev_8", "created_at": "2026-09-03T09:00:00Z",
	  "source_sandbox": "k3j2v0d8p1q7x9r2m5n8b4c6a", "clock_restore": "today",
	  "size_bytes": 4096
	}`
)

func TestEveryMethodSendsItsPathMethodAndBody(t *testing.T) {
	url := "https://3000-abc.e2b.app"
	ttl := 240
	image := "reg.example/env-repo/env-x@sha256:abc"
	ctx := context.Background()

	cases := []struct {
		name       string
		call       func(c *Client) (any, error)
		wantMethod string
		wantPath   string
		// wantBody is the exact JSON sent; "" means no body at all.
		wantBody string
		status   int
		answer   string
		check    func(t *testing.T, got any)
	}{
		{
			name:       "DeviceCode",
			call:       func(c *Client) (any, error) { return c.DeviceCode(ctx, "veris on host") },
			wantMethod: "POST", wantPath: "/v1/device/code",
			wantBody: `{"client_name":"veris on host"}`,
			status:   200,
			answer: `{"user_code":"ABCD-EFGH","device_code":"vdc_secret",
			          "verification_url":"https://studio.veris.ai/connect",
			          "verification_url_complete":"https://studio.veris.ai/connect?code=ABCD-EFGH",
			          "expires_in":900,"interval":5}`,
			check: func(t *testing.T, got any) {
				want := &DeviceCodeResponse{
					UserCode: "ABCD-EFGH", DeviceCode: "vdc_secret",
					VerificationURL:         "https://studio.veris.ai/connect",
					VerificationURLComplete: "https://studio.veris.ai/connect?code=ABCD-EFGH",
					ExpiresIn:               900, Interval: 5,
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "DeviceToken",
			call:       func(c *Client) (any, error) { return c.DeviceToken(ctx, "vdc_secret") },
			wantMethod: "POST", wantPath: "/v1/device/token",
			wantBody: `{"device_code":"vdc_secret"}`,
			status:   200,
			answer:   `{"api_key":"vsk_mi4pa0uo_rest","organization_id":"org_1","key_id":"key_1","key_name":"claude-code · device"}`,
			check: func(t *testing.T, got any) {
				want := &DeviceTokenResponse{APIKey: "vsk_mi4pa0uo_rest", OrganizationID: "org_1", KeyID: "key_1", KeyName: "claude-code · device"}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "Me as a user",
			call:       func(c *Client) (any, error) { return c.Me(ctx) },
			wantMethod: "GET", wantPath: "/v1/me",
			status: 200,
			answer: `{"kind":"user","user":{"id":"usr_1","email":"ada@acme.test","name":"Ada Lovelace"},
			          "organization_id":"org_p",
			          "organizations":[{"id":"org_p","name":"Ada","slug":"ada","kind":"personal","role":"owner"}]}`,
			check: func(t *testing.T, got any) {
				want := &Me{
					Kind: "user", User: &MeUser{ID: "usr_1", Email: "ada@acme.test", Name: "Ada Lovelace"},
					OrganizationID: "org_p",
					Organizations:  []Organization{{ID: "org_p", Name: "Ada", Slug: "ada", Kind: "personal", Role: "owner"}},
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "Me as a key",
			call:       func(c *Client) (any, error) { return c.Me(ctx) },
			wantMethod: "GET", wantPath: "/v1/me",
			status: 200,
			answer: `{"kind":"api_key","user":null,"organization_id":"org_t",
			          "organizations":[{"id":"org_t","name":"Team","slug":"team","kind":"team","role":null}]}`,
			check: func(t *testing.T, got any) {
				me := got.(*Me)
				if me.Kind != "api_key" || me.User != nil || me.Organizations[0].Role != "" {
					t.Errorf("got %+v", me)
				}
			},
		},
		{
			name:       "ListAPIKeys",
			call:       func(c *Client) (any, error) { return c.ListAPIKeys(ctx) },
			wantMethod: "GET", wantPath: "/v1/api-keys",
			status: 200,
			answer: `[{"id":"key_1","name":"rollout-harness · device","key_prefix":"vsk_mi4pa0uo","status":"active",
			           "created_at":"2026-09-01T00:00:00Z","created_by":"person@x.test","expires_at":null}]`,
			check: func(t *testing.T, got any) {
				keys := got.([]APIKey)
				if len(keys) != 1 || keys[0].KeyPrefix != "vsk_mi4pa0uo" || keys[0].CreatedBy != "person@x.test" {
					t.Errorf("got %+v", keys)
				}
				if !keys[0].ExpiresAt.IsZero() || keys[0].CreatedAt.Year() != 2026 {
					t.Errorf("times = %v / %v", keys[0].CreatedAt, keys[0].ExpiresAt)
				}
			},
		},
		{
			name:       "RevokeAPIKey",
			call:       func(c *Client) (any, error) { return c.RevokeAPIKey(ctx, "key_1") },
			wantMethod: "POST", wantPath: "/v1/api-keys/key_1/revoke",
			wantBody: `{}`,
			status:   200,
			answer:   `{"id":"key_1","name":"n","key_prefix":"vsk_x","status":"revoked","created_at":"2026-09-01T00:00:00Z","created_by":"a@b.c"}`,
			check: func(t *testing.T, got any) {
				if k := got.(*APIKey); k.Status != "revoked" || k.ID != "key_1" {
					t.Errorf("got %+v", k)
				}
			},
		},
		{
			name:       "ListServices",
			call:       func(c *Client) (any, error) { return c.ListServices(ctx) },
			wantMethod: "GET", wantPath: "/v1/services",
			status: 200,
			answer: `[{"name":"beta","description":null,"env_hint":null,"routes":null},
			          {"name":"stripe","description":"Payments","env_hint":"STRIPE_API_BASE",
			           "routes":[{"host":"api.stripe.com","paths":null}]}]`,
			check: func(t *testing.T, got any) {
				want := []CatalogService{
					{Name: "beta"},
					{Name: "stripe", Description: "Payments", EnvHint: "STRIPE_API_BASE",
						Routes: []routes.Entry{{Host: "api.stripe.com"}}},
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
			},
		},
		{
			name: "CreateEnvironment",
			call: func(c *Client) (any, error) {
				return c.CreateEnvironment(ctx, "billing", []string{"stripe", "postgres"})
			},
			wantMethod: "POST", wantPath: "/v1/environments",
			wantBody: `{"name":"billing","services":["stripe","postgres"]}`,
			status:   201, answer: environmentJSON,
			check: checkEnvironment,
		},
		{
			name:       "CreateEnvironment unnamed",
			call:       func(c *Client) (any, error) { return c.CreateEnvironment(ctx, "", []string{"stripe"}) },
			wantMethod: "POST", wantPath: "/v1/environments",
			wantBody: `{"services":["stripe"]}`,
			status:   201, answer: environmentJSON,
			check: checkEnvironment,
		},
		{
			name:       "ListEnvironments",
			call:       func(c *Client) (any, error) { return c.ListEnvironments(ctx) },
			wantMethod: "GET", wantPath: "/v1/environments",
			status: 200, answer: `[` + environmentJSON + `]`,
			check: func(t *testing.T, got any) {
				envs := got.([]Environment)
				if len(envs) != 1 {
					t.Fatalf("got %d environments", len(envs))
				}
				checkEnvironment(t, &envs[0])
			},
		},
		{
			name:       "GetEnvironment",
			call:       func(c *Client) (any, error) { return c.GetEnvironment(ctx, envID) },
			wantMethod: "GET", wantPath: "/v1/environments/" + envID,
			status: 200, answer: environmentJSON,
			check: checkEnvironment,
		},
		{
			name:       "DeleteEnvironment",
			call:       func(c *Client) (any, error) { return nil, c.DeleteEnvironment(ctx, envID) },
			wantMethod: "DELETE", wantPath: "/v1/environments/" + envID,
			status: 204,
		},
		{
			name:       "ResetEnvironment clears the pin",
			call:       func(c *Client) (any, error) { return c.ResetEnvironment(ctx, envID, nil) },
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/reset",
			wantBody: `{"baseline_image":null}`,
			status:   200,
			answer:   `{"id":"` + envID + `","name":"billing","services":["stripe"],"created_at":null,"owner":"default","baseline":null}`,
			check: func(t *testing.T, got any) {
				env := got.(*Environment)
				if env.Baseline != nil || !env.CreatedAt.IsZero() || env.Owner != "default" {
					t.Errorf("got %+v", env)
				}
			},
		},
		{
			name:       "ResetEnvironment rolls back",
			call:       func(c *Client) (any, error) { return c.ResetEnvironment(ctx, envID, &image) },
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/reset",
			wantBody: `{"baseline_image":"reg.example/env-repo/env-x@sha256:abc"}`,
			status:   200, answer: environmentJSON,
			check: checkEnvironment,
		},
		{
			name: "CreateSandbox",
			call: func(c *Client) (any, error) {
				return c.CreateSandbox(ctx, envID, CreateSandboxRequest{
					TTLMinutes: &ttl, SnapshotID: strp(snapID), ClientBaseURL: &url,
					Metadata: map[string]string{"run": "billing-rl-07"},
				})
			},
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/sandboxes",
			wantBody: `{"ttl_minutes":240,"snapshot_id":"` + snapID + `","client_base_url":"https://3000-abc.e2b.app","metadata":{"run":"billing-rl-07"}}`,
			status:   201, answer: sandboxJSON,
			check: checkSandbox,
		},
		{
			name:       "GetSandbox",
			call:       func(c *Client) (any, error) { return c.GetSandbox(ctx, sandboxID) },
			wantMethod: "GET", wantPath: "/v1/sandboxes/" + sandboxID,
			status: 200, answer: sandboxJSON,
			check: checkSandbox,
		},
		{
			name:       "GetSandboxServices",
			call:       func(c *Client) (any, error) { return c.GetSandboxServices(ctx, sandboxID) },
			wantMethod: "GET", wantPath: "/v1/sandboxes/" + sandboxID + "/services",
			status: 200,
			answer: `[{"name":"stripe","status":"ready","url":"http://gw/s/x/stripe","control_url":"http://gw/s/x/stripe","env_hint":"STRIPE_API_BASE","routes":null}]`,
			check: func(t *testing.T, got any) {
				want := []ServiceInfo{{Name: "stripe", Status: "ready", URL: "http://gw/s/x/stripe",
					ControlURL: "http://gw/s/x/stripe", EnvHint: "STRIPE_API_BASE"}}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "ListSandboxes",
			call:       func(c *Client) (any, error) { return c.ListSandboxes(ctx, envID) },
			wantMethod: "GET", wantPath: "/v1/environments/" + envID + "/sandboxes",
			status: 200, answer: `[` + sandboxJSON + `]`,
			check: func(t *testing.T, got any) {
				sbs := got.([]Sandbox)
				if len(sbs) != 1 {
					t.Fatalf("got %d sandboxes", len(sbs))
				}
				checkSandbox(t, &sbs[0])
			},
		},
		{
			name:       "DeleteSandbox",
			call:       func(c *Client) (any, error) { return nil, c.DeleteSandbox(ctx, envID, sandboxID) },
			wantMethod: "DELETE", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID,
			status: 204,
		},
		{
			name:       "ResetSandbox",
			call:       func(c *Client) (any, error) { return c.ResetSandbox(ctx, envID, sandboxID) },
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID + "/reset",
			wantBody: `{}`,
			status:   200,
			answer: `{"id":"` + sandboxID + `","ok":false,"services":[
			          {"name":"stripe","ok":true,"detail":{"reset":true}},
			          {"name":"beta","ok":false,"detail":"atomic sandbox reset failed: boom"}]}`,
			check: func(t *testing.T, got any) {
				r := got.(*ResetResponse)
				if r.ID != sandboxID || r.OK || len(r.Services) != 2 {
					t.Fatalf("got %+v", r)
				}
				if !r.Services[0].OK || string(r.Services[0].Detail) != `{"reset":true}` {
					t.Errorf("stripe = %+v", r.Services[0])
				}
				if r.Services[1].OK || string(r.Services[1].Detail) != `"atomic sandbox reset failed: boom"` {
					t.Errorf("beta = %+v", r.Services[1])
				}
			},
		},
		{
			name: "UpdateSandbox sets",
			call: func(c *Client) (any, error) {
				return c.UpdateSandbox(ctx, envID, sandboxID, SandboxUpdate{ClientBaseURL: &url})
			},
			wantMethod: "PATCH", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID,
			wantBody: `{"client_base_url":"https://3000-abc.e2b.app"}`,
			status:   200, answer: sandboxJSON,
			check: checkSandbox,
		},
		{
			name: "UpdateSandbox clears",
			call: func(c *Client) (any, error) {
				return c.UpdateSandbox(ctx, envID, sandboxID, SandboxUpdate{Clear: true})
			},
			wantMethod: "PATCH", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID,
			wantBody: `{"client_base_url":null}`,
			status:   200, answer: sandboxJSON,
			check: checkSandbox,
		},
		{
			name: "PromoteSandbox",
			call: func(c *Client) (any, error) {
				return c.PromoteSandbox(ctx, envID, sandboxID, PromoteRequest{ClockRestore: ClockFrozen, KeepExternalDestinations: true})
			},
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID + "/promote",
			wantBody: `{"clock_restore":"frozen","keep_external_destinations":true}`,
			status:   200,
			answer: `{"environment_id":"` + envID + `","sandbox_id":"` + sandboxID + `",
			          "baseline":{"image":"reg.example/env-repo/env-x@sha256:abc","revision_id":"rev_7",
			                      "promoted_at":"2026-09-03T10:00:00Z","source_sandbox":"` + sandboxID + `"},
			          "clock_restore":"frozen","size_bytes":123456,"curator_clock_restored":false,
			          "scrubbed":{"stripe":["deliveries","request_log"],"beta":[]}}`,
			check: func(t *testing.T, got any) {
				p := got.(*PromoteResponse)
				if p.Baseline.Image != "reg.example/env-repo/env-x@sha256:abc" || p.ClockRestore != "frozen" ||
					p.SizeBytes != 123456 || p.CuratorClockRestored {
					t.Errorf("got %+v", p)
				}
				if !reflect.DeepEqual(p.Scrubbed, map[string][]string{"stripe": {"deliveries", "request_log"}, "beta": {}}) {
					t.Errorf("scrubbed = %v", p.Scrubbed)
				}
			},
		},
		{
			name:       "PromoteSandbox defaults",
			call:       func(c *Client) (any, error) { return c.PromoteSandbox(ctx, envID, sandboxID, PromoteRequest{}) },
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID + "/promote",
			wantBody: `{"keep_external_destinations":false}`,
			status:   200,
			answer:   `{"environment_id":"e","sandbox_id":"s","baseline":{"image":"i","revision_id":"r","promoted_at":null,"source_sandbox":"s"},"clock_restore":"today","size_bytes":1}`,
			check: func(t *testing.T, got any) {
				if p := got.(*PromoteResponse); p.ClockRestore != "today" || !p.Baseline.PromotedAt.IsZero() {
					t.Errorf("got %+v", p)
				}
			},
		},
		{
			name:       "EgressCredential",
			call:       func(c *Client) (any, error) { return c.EgressCredential(ctx, envID, sandboxID) },
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID + "/egress-credential",
			wantBody: `{}`,
			status:   201,
			answer: `{"socks_address":"gw.dev.api.veris.ai:1080","username":"v1.` + sandboxID + `",
			          "ca_pem":"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
			          "canary_host":"canary.gw.dev.api.veris.ai",
			          "connect_address":"gw.dev.api.veris.ai:8080",
			          "http_proxy_url":"http://v1.` + sandboxID + `:` + sandboxID + `@gw.dev.api.veris.ai:8080",
			          "min_sdk":"2.0.0"}`,
			check: func(t *testing.T, got any) {
				want := &EgressCredential{
					SocksAddress: "gw.dev.api.veris.ai:1080", Username: "v1." + sandboxID,
					CAPEM:      "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
					CanaryHost: "canary.gw.dev.api.veris.ai", ConnectAddress: "gw.dev.api.veris.ai:8080",
					HTTPProxyURL: "http://v1." + sandboxID + ":" + sandboxID + "@gw.dev.api.veris.ai:8080",
					MinSDK:       "2.0.0",
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
			},
		},
		{
			name:       "EgressCredential socks only",
			call:       func(c *Client) (any, error) { return c.EgressCredential(ctx, envID, sandboxID) },
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/sandboxes/" + sandboxID + "/egress-credential",
			wantBody: `{}`,
			status:   201,
			answer:   `{"socks_address":"gw:1080","username":"v1.x","ca_pem":"pem","canary_host":"c","connect_address":null,"http_proxy_url":null,"min_sdk":"2.0.0"}`,
			check: func(t *testing.T, got any) {
				if e := got.(*EgressCredential); e.ConnectAddress != "" || e.HTTPProxyURL != "" {
					t.Errorf("got %+v", e)
				}
			},
		},
		{
			name:       "ListSnapshots",
			call:       func(c *Client) (any, error) { return c.ListSnapshots(ctx, envID) },
			wantMethod: "GET", wantPath: "/v1/environments/" + envID + "/snapshots",
			status: 200, answer: `[` + snapshotJSON + `]`,
			check: func(t *testing.T, got any) {
				snaps := got.([]Snapshot)
				if len(snaps) != 1 {
					t.Fatalf("got %d snapshots", len(snaps))
				}
				checkSnapshot(t, &snaps[0])
			},
		},
		{
			name: "CreateSnapshot",
			call: func(c *Client) (any, error) {
				return c.CreateSnapshot(ctx, envID, CreateSnapshotRequest{SandboxID: sandboxID, Name: "before-run-3"})
			},
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/snapshots",
			wantBody: `{"sandbox_id":"` + sandboxID + `","name":"before-run-3","keep_external_destinations":false}`,
			status:   201,
			answer:   `{"snapshot":` + snapshotJSON + `,"curator_clock_restored":true,"scrubbed":{"stripe":["deliveries"]}}`,
			check: func(t *testing.T, got any) {
				r := got.(*SnapshotResponse)
				checkSnapshot(t, &r.Snapshot)
				if !r.CuratorClockRestored || !reflect.DeepEqual(r.Scrubbed, map[string][]string{"stripe": {"deliveries"}}) {
					t.Errorf("got %+v", r)
				}
			},
		},
		{
			name: "CreateSnapshot every field",
			call: func(c *Client) (any, error) {
				return c.CreateSnapshot(ctx, envID, CreateSnapshotRequest{
					SandboxID: sandboxID, Name: "n", ClockRestore: ClockRebase, KeepExternalDestinations: true})
			},
			wantMethod: "POST", wantPath: "/v1/environments/" + envID + "/snapshots",
			wantBody: `{"sandbox_id":"` + sandboxID + `","name":"n","clock_restore":"rebase","keep_external_destinations":true}`,
			status:   201,
			answer:   `{"snapshot":` + snapshotJSON + `}`,
			check: func(t *testing.T, got any) {
				checkSnapshot(t, &got.(*SnapshotResponse).Snapshot)
			},
		},
		{
			name:       "DeleteSnapshot",
			call:       func(c *Client) (any, error) { return nil, c.DeleteSnapshot(ctx, envID, snapID) },
			wantMethod: "DELETE", wantPath: "/v1/environments/" + envID + "/snapshots/" + snapID,
			status: 204,
		},
		{
			name:       "Healthz",
			call:       func(c *Client) (any, error) { return c.Healthz(ctx) },
			wantMethod: "GET", wantPath: "/healthz",
			status: 200, answer: `{"status":"ok","checkout":"3f9a1c"}`,
			check: func(t *testing.T, got any) {
				if !reflect.DeepEqual(got, &Healthz{Status: "ok", Checkout: "3f9a1c"}) {
					t.Errorf("got %+v", got)
				}
			},
		},
		{
			name:       "GatewayHealth",
			call:       func(c *Client) (any, error) { return c.GatewayHealth(ctx) },
			wantMethod: "GET", wantPath: "/v1/gateway/health",
			status: 200, answer: `{"available":true,"canary_host":"canary.gw.dev.api.veris.ai"}`,
			check: func(t *testing.T, got any) {
				if !reflect.DeepEqual(got, &GatewayHealth{Available: true, CanaryHost: "canary.gw.dev.api.veris.ai"}) {
					t.Errorf("got %+v", got)
				}
			},
		},
		{
			name:       "GatewayHealth absent",
			call:       func(c *Client) (any, error) { return c.GatewayHealth(ctx) },
			wantMethod: "GET", wantPath: "/v1/gateway/health",
			status: 200, answer: `{"available":false,"canary_host":null}`,
			check: func(t *testing.T, got any) {
				if !reflect.DeepEqual(got, &GatewayHealth{}) {
					t.Errorf("got %+v", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.answer == "" {
					w.WriteHeader(tc.status)
					return
				}
				respond(w, tc.status, tc.answer)
			})
			got, err := tc.call(c)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if rec.count() != 1 {
				t.Fatalf("requests = %d, want 1", rec.count())
			}
			sent := rec.last()
			if sent.Method != tc.wantMethod || sent.Path != tc.wantPath {
				t.Errorf("sent %s %s, want %s %s", sent.Method, sent.Path, tc.wantMethod, tc.wantPath)
			}
			if sent.Body != tc.wantBody {
				t.Errorf("body = %s, want %s", sent.Body, tc.wantBody)
			}
			if tc.wantBody != "" && sent.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q", sent.Header.Get("Content-Type"))
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func strp(s string) *string { return &s }

func checkEnvironment(t *testing.T, got any) {
	t.Helper()
	env, ok := got.(*Environment)
	if !ok {
		t.Fatalf("got %T", got)
	}
	want := &Environment{
		ID: envID, Name: "billing", Services: []string{"stripe", "postgres"},
		CreatedAt: Time{time.Date(2026, 9, 1, 8, 15, 30, 123456000, time.UTC)},
		Owner:     "org_1",
		Baseline: &EnvironmentBaseline{
			Image:         "reg.example/env-repo/env-e1v2i3r4o5n6m7e8n9t0a1b2c@sha256:abc",
			RevisionID:    "rev_7",
			PromotedAt:    Time{time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
			SourceSandbox: sandboxID,
		},
	}
	if !env.CreatedAt.Equal(want.CreatedAt.Time) || !env.Baseline.PromotedAt.Equal(want.Baseline.PromotedAt.Time) {
		t.Errorf("times = %v / %v", env.CreatedAt, env.Baseline.PromotedAt)
	}
	env.CreatedAt, env.Baseline.PromotedAt = want.CreatedAt, want.Baseline.PromotedAt
	if !reflect.DeepEqual(env, want) {
		t.Errorf("got %+v, want %+v", env, want)
	}
}

func checkSandbox(t *testing.T, got any) {
	t.Helper()
	sb, ok := got.(*Sandbox)
	if !ok {
		t.Fatalf("got %T", got)
	}
	if sb.ID != sandboxID || sb.EnvironmentID != envID || sb.Status != StatusProvisioning {
		t.Errorf("identity = %s %s %s", sb.ID, sb.EnvironmentID, sb.Status)
	}
	if !sb.CreatedAt.Equal(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)) ||
		!sb.ExpiresAt.Equal(time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("times = %v / %v", sb.CreatedAt, sb.ExpiresAt)
	}
	if sb.FailureReason != "" || sb.SnapshotID != "" {
		t.Errorf("nulls decoded as %q / %q", sb.FailureReason, sb.SnapshotID)
	}
	if !reflect.DeepEqual(sb.Metadata, map[string]string{"run": "billing-rl-07", "epoch": "3"}) {
		t.Errorf("metadata = %v", sb.Metadata)
	}
	wantServices := []ServiceInfo{
		{Name: "stripe", Status: ServicePending,
			URL:        "http://203.0.113.10/s/" + sandboxID + "/stripe",
			ControlURL: "http://203.0.113.10/s/" + sandboxID + "/stripe",
			EnvHint:    "STRIPE_API_BASE",
			Routes:     []routes.Entry{{Host: "api.stripe.com"}}},
		{Name: "postgres", Status: ServiceReady,
			URL:        "postgresql://" + sandboxID + ":x@pg.test:5432/" + sandboxID,
			ControlURL: "http://203.0.113.10/s/" + sandboxID + "/postgres",
			EnvHint:    "DATABASE_URL"},
	}
	if !reflect.DeepEqual(sb.Services, wantServices) {
		t.Errorf("services = %+v, want %+v", sb.Services, wantServices)
	}
}

func checkSnapshot(t *testing.T, s *Snapshot) {
	t.Helper()
	if s.ID != snapID || s.EnvironmentID != envID || s.Name != "before-run-3" ||
		s.Image != "reg.example/env-repo/env-e1v2i3r4o5n6m7e8n9t0a1b2c@sha256:def" ||
		s.RevisionID != "rev_8" || s.SourceSandbox != sandboxID || s.ClockRestore != "today" || s.SizeBytes != 4096 {
		t.Errorf("got %+v", s)
	}
	if !s.CreatedAt.Equal(time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at = %v", s.CreatedAt)
	}
}

// A decoded answer re-encodes with the same field names, which is what --json
// consumers and the design's "mirror models.py" rule both rely on.
func TestJSONTagsMirrorModelsPy(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want []string
	}{
		{"Environment", Environment{}, []string{"id", "name", "services", "created_at", "owner", "baseline"}},
		{"EnvironmentBaseline", EnvironmentBaseline{}, []string{"image", "revision_id", "promoted_at", "source_sandbox"}},
		{"Sandbox", Sandbox{}, []string{"id", "environment_id", "status", "created_at", "expires_at", "services", "failure_reason", "metadata", "snapshot_id"}},
		{"ServiceInfo", ServiceInfo{}, []string{"name", "status", "url", "control_url", "env_hint", "routes"}},
		{"Snapshot", Snapshot{}, []string{"id", "environment_id", "name", "image", "revision_id", "created_at", "source_sandbox", "clock_restore", "size_bytes"}},
		{"SnapshotResponse", SnapshotResponse{}, []string{"snapshot", "curator_clock_restored", "scrubbed"}},
		{"PromoteResponse", PromoteResponse{}, []string{"environment_id", "sandbox_id", "baseline", "clock_restore", "size_bytes", "curator_clock_restored", "scrubbed"}},
		{"ResetResponse", ResetResponse{}, []string{"id", "ok", "services"}},
		{"ServiceResetResult", ServiceResetResult{}, []string{"name", "ok", "detail"}},
		{"EgressCredential", EgressCredential{}, []string{"socks_address", "username", "ca_pem", "canary_host", "connect_address", "http_proxy_url", "min_sdk"}},
		{"GatewayHealth", GatewayHealth{}, []string{"available", "canary_host"}},
		{"CatalogService", CatalogService{}, []string{"name", "description", "env_hint", "routes"}},
		{"Healthz", Healthz{}, []string{"status", "checkout"}},
		{"APIKey", APIKey{}, []string{"id", "name", "key_prefix", "status", "created_at", "created_by", "expires_at"}},
		{"Me", Me{}, []string{"kind", "user", "organization_id", "organizations"}},
		{"MeUser", MeUser{}, []string{"id", "email", "name"}},
		{"Organization", Organization{}, []string{"id", "name", "slug", "kind", "role"}},
		{"DeviceCodeResponse", DeviceCodeResponse{}, []string{"user_code", "device_code", "verification_url", "verification_url_complete", "expires_in", "interval"}},
		{"DeviceTokenResponse", DeviceTokenResponse{}, []string{"api_key", "organization_id", "key_id", "key_name"}},
		{"DeviceCodeRequest", DeviceCodeRequest{}, []string{"client_name"}},
		{"DeviceTokenRequest", DeviceTokenRequest{}, []string{"device_code"}},
		{"CreateEnvironmentRequest", CreateEnvironmentRequest{Name: "n"}, []string{"name", "services"}},
		{"ResetEnvironmentRequest", ResetEnvironmentRequest{}, []string{"baseline_image"}},
		{"PromoteRequest", PromoteRequest{ClockRestore: "today"}, []string{"clock_restore", "keep_external_destinations"}},
		{"CreateSnapshotRequest", CreateSnapshotRequest{Name: "n", ClockRestore: "today"}, []string{"sandbox_id", "name", "clock_restore", "keep_external_destinations"}},
		{"CreateSandboxRequest", CreateSandboxRequest{TTLMinutes: new(int), SnapshotID: strp(""), ClientBaseURL: strp(""), Metadata: map[string]string{"k": "v"}},
			[]string{"ttl_minutes", "snapshot_id", "client_base_url", "metadata"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(b, &fields); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.want {
				if _, ok := fields[name]; !ok {
					t.Errorf("missing %q in %s", name, b)
				}
			}
			if len(fields) != len(tc.want) {
				t.Errorf("%s has %d fields, want %d: %s", tc.name, len(fields), len(tc.want), b)
			}
		})
	}
}
