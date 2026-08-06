package trust

// Java trust distribution.
//
// The JVM reads no proxy environment variable and no PEM CA variable of any
// kind, so covering Java takes two artifacts this file knows how to build with
// the JDK's own keytool:
//
//   - a truststore: a copy of the JDK's cacerts with the Veris CA imported,
//     wired in via -Djavax.net.ssl.trustStore inside JAVA_TOOL_OPTIONS;
//   - an injection into an app-managed keystore, for services that load their
//     own trust material from disk (a k8s-mounted keystore.p12 is the common
//     shape) and never consult the JVM default truststore. Some of those apps
//     wrap their keystore in a custom trust manager whose fallback to the JVM
//     default is broken in practice, so putting the CA where the app actually
//     looks is the only reliable move.
//
// keytool is shelled out to rather than reimplemented: JKS is a proprietary
// format, and anyone who needs a Java truststore has a JDK.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultJavaTrustStoreName is the filename `trust --java` writes under the CA
// directory, and the place `env` looks for an already-built truststore so the
// developer does not have to pass --java-truststore on every invocation.
const DefaultJavaTrustStoreName = "java-truststore"

// BuildJavaTrustStore copies the JDK's cacerts to outPath and imports the
// Veris CA into the copy, so the result trusts both the real internet and the
// interception proxy. It returns the cacerts path that was used, so the caller
// can report which JDK the truststore came from.
//
// storePass must be the cacerts password, which is "changeit" on every stock
// JDK; the copy keeps it.
func BuildJavaTrustStore(jdkDir, caCertPath, outPath, storePass string) (string, error) {
	keytool, err := findKeytool(jdkDir)
	if err != nil {
		return "", err
	}
	cacerts, err := findCacerts(jdkDir, keytool)
	if err != nil {
		return "", err
	}

	// Always start from a fresh copy rather than re-importing into an old
	// build, so a rotated CA can never coexist with a stale one.
	if err := copyFile(cacerts, outPath); err != nil {
		return "", fmt.Errorf("copy cacerts: %w", err)
	}
	if err := runKeytool(keytool,
		"-importcert", "-noprompt",
		"-alias", "veris-ca",
		"-file", caCertPath,
		"-keystore", outPath,
		"-storepass", storePass,
	); err != nil {
		return "", err
	}
	return cacerts, nil
}

// InjectCA imports the Veris CA into an existing keystore that an application
// loads itself. Re-running is safe: a previous veris-ca entry is replaced, not
// duplicated.
func InjectCA(jdkDir, caCertPath, keystorePath, storePass string) error {
	keytool, err := findKeytool(jdkDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(keystorePath); err != nil {
		return fmt.Errorf("keystore %s: %w", keystorePath, err)
	}

	// Delete any previous entry first; the error is ignored because "alias
	// does not exist" is the expected first-run outcome.
	_ = runKeytool(keytool,
		"-delete",
		"-alias", "veris-ca",
		"-keystore", keystorePath,
		"-storepass", storePass,
	)
	return runKeytool(keytool,
		"-importcert", "-noprompt",
		"-alias", "veris-ca",
		"-file", caCertPath,
		"-keystore", keystorePath,
		"-storepass", storePass,
	)
}

func findKeytool(jdkDir string) (string, error) {
	exe := "keytool"
	if runtime.GOOS == "windows" {
		exe = "keytool.exe"
	}
	if jdkDir != "" {
		p := filepath.Join(jdkDir, "bin", exe)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("no keytool under %s", jdkDir)
	}
	if home := os.Getenv("JAVA_HOME"); home != "" {
		p := filepath.Join(home, "bin", exe)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath(exe); err == nil {
		return p, nil
	}
	return "", errors.New("keytool not found: set $JAVA_HOME or pass --jdk")
}

// findCacerts locates the JDK's default truststore. The keytool path is a
// hint: resolving its symlink and stepping up two directories lands on the
// JDK home even when keytool was only found on $PATH.
func findCacerts(jdkDir, keytoolPath string) (string, error) {
	var roots []string
	if jdkDir != "" {
		roots = append(roots, jdkDir)
	}
	if home := os.Getenv("JAVA_HOME"); home != "" {
		roots = append(roots, home)
	}
	if keytoolPath != "" {
		if resolved, err := filepath.EvalSymlinks(keytoolPath); err == nil {
			roots = append(roots, filepath.Dir(filepath.Dir(resolved)))
		}
	}
	for _, r := range roots {
		for _, rel := range []string{
			filepath.Join("lib", "security", "cacerts"),
			// JDK 8 layout.
			filepath.Join("jre", "lib", "security", "cacerts"),
		} {
			p := filepath.Join(r, rel)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("cannot locate the JDK cacerts file: set $JAVA_HOME or pass --jdk")
}

func runKeytool(keytool string, args ...string) error {
	out, err := exec.Command(keytool, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("keytool %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
