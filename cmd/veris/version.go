package main

import (
	"context"
	"fmt"
	"time"

	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
)

// versionProbeTimeout is how long `version` waits for the control plane's
// /healthz before answering with the binary's version alone.
const versionProbeTimeout = 3 * time.Second

// versionCommand prints the binary version and, when a control plane is
// resolved and answers, that plane's health fields on a second line.
func versionCommand() *cli.Command {
	return &cli.Command{
		Name:    "version",
		Summary: "Print the version, and the control plane's when one answers",
		Usage:   "veris version [--json]",
		Help: `The binary's version is the first line and always prints. When a control
plane is resolved -- a profile with a key, --api-base or VERIS_API_BASE --
and answers /healthz within 3 s, a second line names it and what it
reports. An unreachable plane changes nothing, and the exit is 0 either way.`,
		Run: cmdVersion,
	}
}

// versionReport is --json's body. ControlPlane is null when no plane was
// asked or none answered.
type versionReport struct {
	Version      string       `json:"version"`
	ControlPlane *planeHealth `json:"control_plane"`
}

// planeHealth is what /healthz said, and where.
type planeHealth struct {
	APIBase  string `json:"api_base"`
	Status   string `json:"status"`
	Checkout string `json:"checkout,omitempty"`
}

func cmdVersion(ctx *cli.Context, _ []string) error {
	g := ctx.Globals
	if g == nil {
		g = &cli.Globals{}
	}
	plane := controlPlaneHealth(ctx)
	if g.JSON {
		return printJSON(ctx.Stdout, versionReport{Version: version, ControlPlane: plane})
	}
	fmt.Fprintln(ctx.Stdout, version)
	if plane == nil || g.Quiet {
		return nil
	}
	line := plane.Status
	if plane.Checkout != "" {
		line += " · checkout " + plane.Checkout
	}
	fmt.Fprintf(ctx.Stdout, "control plane %s · %s\n", plane.APIBase, line)
	return nil
}

// controlPlaneHealth asks the resolved plane for its /healthz, or returns nil.
// version is the one command that must answer on any machine, so nothing on
// this path is an error: a profiles file that will not parse, a plane that
// is down, a slow network all leave the binary's version on its own. A plane
// nobody named -- the default base with no key on the machine -- is not
// asked, so a fresh install's `veris version` makes no network call.
func controlPlaneHealth(ctx *cli.Context) *planeHealth {
	s, err := newSession(ctx, "", "")
	if err != nil {
		return nil
	}
	if s.res.APIKey == "" && s.res.APIBaseSource == cfg.SourceDefault {
		return nil
	}
	probe, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	h, err := s.plane().Healthz(probe)
	if err != nil {
		return nil
	}
	return &planeHealth{APIBase: s.res.APIBase, Status: h.Status, Checkout: h.Checkout}
}
