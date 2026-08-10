package main

// defaultProxyUID is the uid the proxy drops to and the kernel redirect
// exempts. One number, named once, because the exemption and the drop must
// agree or interception silently covers nothing.
const defaultProxyUID = 14741

// setupOptions is what `serve --transparent` needs in order to stand itself
// up inside a container: which uid to become, which ports the redirect should
// point at, and which directories must be writable once it is no longer root.
type setupOptions struct {
	UID              int
	TransparentHTTP  string
	TransparentHTTPS string
	// Writable are every directory the proxy must still be able to write to
	// after it stops being root: its CA, its sandbox cache, and wherever the
	// handoff files are going.
	Writable []string
	CAPath   string
	// RedirectExternal says the kernel redirect is somebody else's job -- the
	// container entrypoint installs it after starting an already-dropped
	// proxy, so the proxy must neither install nor demand it.
	RedirectExternal bool
}
