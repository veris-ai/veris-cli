package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

const clockPath = "/v1/environments/" + envID + "/sandboxes/" + sandboxID + "/clock"

func TestClockGetReadsTheSingleton(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `{"id":1,"mode":"frozen","offset_seconds":0,"frozen_time":1772355600}`)
	})
	got, err := c.GetSandboxClock(context.Background(), envID, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	frozen := int64(1772355600)
	want := &SandboxClock{ID: 1, Mode: ClockModeFrozen, OffsetSeconds: 0, FrozenTime: &frozen}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	last := rec.last()
	if last.Method != "GET" || last.Path != clockPath {
		t.Errorf("sent %s %s, want GET %s", last.Method, last.Path, clockPath)
	}
	if last.Header.Get("X-API-Key") == "" {
		t.Error("the key was not sent")
	}
}

func TestClockGetReadsALiveClock(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `{"id":1,"mode":"live","offset_seconds":604800,"frozen_time":null}`)
	})
	got, err := c.GetSandboxClock(context.Background(), envID, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ClockModeLive || got.OffsetSeconds != 604800 || got.FrozenTime != nil {
		t.Errorf("got %+v", got)
	}
}

func TestClockSetSendsOnlyTheFieldsNamed(t *testing.T) {
	live, frozen := ClockModeLive, ClockModeFrozen
	zero, week, hold := int64(0), int64(604800), int64(1772355600)
	cases := []struct {
		name string
		req  SetSandboxClockRequest
		want string
	}{
		{"freeze", SetSandboxClockRequest{Mode: &frozen, FrozenTime: &hold}, `{"frozen_time":1772355600,"mode":"frozen"}`},
		{"offset", SetSandboxClockRequest{Mode: &live, OffsetSeconds: &week}, `{"mode":"live","offset_seconds":604800}`},
		{"live clears the hold", SetSandboxClockRequest{Mode: &live, OffsetSeconds: &zero, ClearFrozenTime: true}, `{"frozen_time":null,"mode":"live","offset_seconds":0}`},
		{"offset alone", SetSandboxClockRequest{OffsetSeconds: &week}, `{"offset_seconds":604800}`},
		{"nothing", SetSandboxClockRequest{}, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
				respond(w, 200, `{"clock":{"id":1,"mode":"live","offset_seconds":604800,"frozen_time":null},"warnings":["clock: time moved backwards (10 -> 5). Data created before this change now lies in the future"]}`)
			})
			got, err := c.SetSandboxClock(context.Background(), envID, sandboxID, tc.req)
			if err != nil {
				t.Fatal(err)
			}
			last := rec.last()
			if last.Method != "PATCH" || last.Path != clockPath {
				t.Errorf("sent %s %s, want PATCH %s", last.Method, last.Path, clockPath)
			}
			if last.Body != tc.want {
				t.Errorf("body %s, want %s", last.Body, tc.want)
			}
			if last.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type %q", last.Header.Get("Content-Type"))
			}
			if got.Clock.Mode != ClockModeLive || got.Clock.OffsetSeconds != 604800 {
				t.Errorf("clock %+v", got.Clock)
			}
			if len(got.Warnings) != 1 || got.Warnings[0][:33] != "clock: time moved backwards (10 -" {
				t.Errorf("warnings %q", got.Warnings)
			}
		})
	}
}

func TestClockSetRefusalsBecomeErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantDetail string
		wantReason []string
	}{
		{"unknown sandbox", 404, `{"detail":"sandbox k3j2v0d8p1q7x9r2m5n8b4c6a not found"}`, "sandbox k3j2v0d8p1q7x9r2m5n8b4c6a not found", nil},
		{"the world's refusal", 422, `{"detail":["clock: mode=frozen requires frozen_time to be set"]}`, "clock: mode=frozen requires frozen_time to be set", []string{"clock: mode=frozen requires frozen_time to be set"}},
		{"pydantic's refusal", 422, `{"detail":[{"loc":["body"],"msg":"Value error, set at least one clock field","type":"value_error"}]}`, "Value error, set at least one clock field", []string{"Value error, set at least one clock field"}},
		{"coordinator down", 502, `{"detail":"sandbox clock control is unavailable"}`, "sandbox clock control is unavailable", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
				respond(w, tc.status, tc.body)
			})
			offset := int64(1)
			_, err := c.SetSandboxClock(context.Background(), envID, sandboxID, SetSandboxClockRequest{OffsetSeconds: &offset})
			if !IsStatus(err, tc.status) {
				t.Fatalf("err = %v, want status %d", err, tc.status)
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err %T", err)
			}
			if e.Detail != tc.wantDetail {
				t.Errorf("detail %q, want %q", e.Detail, tc.wantDetail)
			}
			if !reflect.DeepEqual(e.Reasons, tc.wantReason) {
				t.Errorf("reasons %q, want %q", e.Reasons, tc.wantReason)
			}
			// A PATCH is sent exactly once, whatever the answer: the world
			// may have taken the change before the load balancer answered.
			if rec.count() != 1 {
				t.Errorf("%d requests sent, want 1", rec.count())
			}
		})
	}
}

func TestClockResponseReEncodesAsSent(t *testing.T) {
	raw := `{"clock":{"id":1,"mode":"frozen","offset_seconds":0,"frozen_time":1772355600},"warnings":[]}`
	var res SetSandboxClockResponse
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != raw {
		t.Errorf("re-encoded as %s, want %s", out, raw)
	}
}
