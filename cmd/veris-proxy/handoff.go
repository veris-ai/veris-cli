package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/veris-ai/veris-proxy/internal/ca"
	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/proxy"
	"github.com/veris-ai/veris-proxy/internal/trust"
)

// Startup artifacts a supervisor reads: the environment the command under test
// needs, and an edge-triggered signal that every listener is bound.
//
// Both exist because the container entrypoint is POSIX sh in an arbitrary
// customer image. It cannot poll a port -- debian-slim and python-slim carry no
// curl, no wget, no nc, and sh has no /dev/tcp -- and it must not have to parse
// log output, which would make wording an API.
//
// Written by the running server rather than computed by a second command, so
// they describe what actually happened: --listen :0 resolves to a real port
// here, and the CA is the one already loaded rather than one re-read from disk.

// writeEnvFile records the interception environment for whoever starts the
// command under test.
//
// Two formats, because the two callers cannot read each other's. The container
// entrypoint sources shell; `docker run --env-file` takes bare KEY=value and
// treats quotes as literal characters, so a POSIX file handed to it sets
// variables whose values begin with an apostrophe.
func writeEnvFile(path, format string, material trust.Material, trustOnly bool, running *proxy.Running, cfg *config.Config, publicURL string) error {
	vars := trust.Build(trust.Options{
		// The bound address, not the requested one. With --listen :0 the
		// configured value names no port at all.
		ProxyURL:            running.ProxyURL(),
		CACertPath:          material.CertPath,
		CABundlePath:        material.BundlePath,
		JavaTrustStore:      material.JKSPath,
		JavaTrustStorePass:  trustStorePassword,
		SandboxID:           cfg.SandboxID,
		CanaryToken:         cfg.CanaryToken,
		NoProxy:             cfg.AllowPassthrough,
		TrustOnly:           trustOnly,
		NodeAcceptsEnvProxy: !trustOnly && nodeAcceptsEnvProxy(),
		PublicURL:           publicURL,
	})

	var buf []byte
	switch format {
	case "docker":
		buf = append(buf, "# Written by veris-proxy serve --write-env --env-format docker.\n"...)
		buf = append(buf, "# Pass to `docker run --env-file`. Values are literal: no quoting.\n"...)
		for _, v := range vars {
			// No append form here. --env-file cannot reference an existing
			// value, and NODE_OPTIONS is the only variable that wants to, so
			// it is set outright -- which is right for a fresh container and
			// wrong for a shell, hence two formats.
			buf = append(buf, fmt.Sprintf("%s=%s\n", v.Name, v.Value)...)
		}
	case "", "posix":
		buf = append(buf, "# Written by veris-proxy serve --write-env. Source, do not edit.\n"...)
		for _, v := range vars {
			if v.Append {
				// NODE_OPTIONS is the case that matters: replacing it would
				// drop whatever the image or the developer already put there.
				buf = append(buf, fmt.Sprintf("export %s=\"${%s:+$%s }%s\"\n",
					v.Name, v.Name, v.Name, shellEscapeDouble(v.Value))...)
				continue
			}
			buf = append(buf, fmt.Sprintf("export %s=%s\n", v.Name, shellQuoteSingle(v.Value))...)
		}
	default:
		return fmt.Errorf("unknown env format %q (want posix or docker)", format)
	}
	// The docker file is read by `docker run --env-file` as the HOST user, who
	// is neither the proxy's uid nor root. 0600 there means permission denied
	// on native Linux, where the bind mount does not mask ownership. It holds
	// a canary and file paths, not a credential.
	mode := os.FileMode(0o600)
	if format == "docker" {
		mode = 0o644
	}
	return writeFileAtomic(path, buf, mode)
}

// trustStorePassword is the JDK's own cacerts password. A truststore holds no
// secret, and using anything else would mean telling every client a new one.
const trustStorePassword = "changeit"

// publishTrust writes the certificate, the public-roots bundle and the JVM
// truststore where the code under test can read them, and returns those paths.
//
// dst, when set, is where the READER will look -- a directory shared with
// another container. It is not the CA directory, which also holds the private
// key that the workload has no business being able to sign with.
//
// Run before the readiness marker, because whoever waits on that marker reads
// these paths straight out of the environment file, and a file published
// afterwards is one the reader can miss.
func publishTrust(dst string, authority *ca.CA) (trust.Material, error) {
	dir := filepath.Dir(authority.CertPath())
	if dst != "" {
		dir = filepath.Dir(dst)
	}
	m, err := trust.Publish(dir, authority.CertPEM(), trustStorePassword)
	if err != nil {
		return trust.Material{}, err
	}
	// Honour an explicitly named certificate path even when it is not the
	// conventional basename, since a caller may have already told someone else
	// where to look.
	if dst != "" && dst != m.CertPath {
		if err := writeFileAtomic(dst, authority.CertPEM(), 0o644); err != nil {
			return trust.Material{}, err
		}
		m.CertPath = dst
	}
	return m, nil
}

// nodeAcceptsEnvProxy reports whether the node that a local run would launch
// tolerates --use-env-proxy inside NODE_OPTIONS.
//
// Measured rather than inferred from a version string. Node did not gain the
// flag until 22.21 and 24.5, and an older one does not ignore it: it prints
// "--use-env-proxy is not allowed in NODE_OPTIONS" and exits before running a
// line of the program. Setting it blind breaks every Node command the run was
// supposed to be instrumenting -- measured on Node 20.20 and 23.11, which both
// refuse, against 22.23 and 24.19, which accept.
func nodeAcceptsEnvProxy() bool {
	node, err := exec.LookPath("node")
	if err != nil {
		return false
	}
	cmd := exec.Command(node, "-e", "")
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=--use-env-proxy")
	return cmd.Run() == nil
}

// writeReadyFile is the last thing startup does, so its existence means every
// listener is bound and the environment file is complete. Written atomically
// into the same directory so a reader never sees a partial file, and never
// sees the marker before what it vouches for.
func writeReadyFile(path string, running *proxy.Running) error {
	body := fmt.Sprintf("proxy=%s\npid=%d\n", running.ProxyURL(), os.Getpid())
	return writeFileAtomic(path, []byte(body), 0o600)
}

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".veris-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	// Rename within one directory is atomic, so a reader sees the whole file
	// or no file. A writer that appended in place would let the entrypoint
	// source half an environment.
	return os.Rename(tmp.Name(), path)
}

func shellQuoteSingle(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, `'\''`...)
			continue
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}

func shellEscapeDouble(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' || s[i] == '$' || s[i] == '`' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
