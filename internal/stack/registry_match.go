package stack

import (
	"strings"

	"github.com/arbianshkodra/accelero/internal/store"
)

// defaultRegistryHost is what an image without an explicit registry
// part resolves to. Matches Docker's behaviour: `nginx:1.27` and
// `library/nginx` both pull from the Docker Hub at docker.io.
const defaultRegistryHost = "docker.io"

// registryFromImage returns the registry hostname embedded in an image
// reference, falling back to docker.io when the reference has no
// explicit registry part.
//
// Docker's parsing rule: the first path segment is treated as a
// registry when it contains a `.`, contains a `:`, or is `localhost`.
// Otherwise it's a Hub namespace (e.g. `library/nginx`). The matcher
// reproduces that rule rather than calling a parser dependency, which
// keeps the deployer free of containerd/distribution imports.
//
// Examples:
//
//	"nginx:1.27"                      → "docker.io"
//	"library/nginx"                   → "docker.io"
//	"foo/bar:tag"                     → "docker.io"
//	"ghcr.io/foo/bar"                 → "ghcr.io"
//	"registry.example.com:5000/x"     → "registry.example.com:5000"
//	"localhost:5000/x"                → "localhost:5000"
//	"index.docker.io/library/nginx"   → "index.docker.io"
func registryFromImage(image string) string {
	// Strip tag/digest — they can't contain '/' so anything before the
	// first slash is the candidate registry segment.
	first, rest, hasRest := strings.Cut(image, "/")
	if !hasRest {
		// No slash at all: an image like "nginx:tag" — Hub default.
		return defaultRegistryHost
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	_ = rest
	return defaultRegistryHost
}

// pickRegistryAuth returns the (username, password, server) tuple to
// use when pulling imageRef. It walks the candidate list in order
// (newer/explicit entries first, legacy single-credential last) and
// returns the first match by registry hostname.
//
// An empty username in the result means "no auth, anonymous pull" —
// matches Docker's behaviour for public images. Callers should still
// pass the imageRef-derived server through as the ServerAddress on
// the auth config; the registry chooses to ignore or honour it.
//
// Returned server is always the resolved registry from imageRef, not
// the cred-list entry's server. That keeps the registry hostname in
// the daemon's auth header consistent with the image being pulled.
func pickRegistryAuth(imageRef string, candidates []*store.StackRegistry, legacy *store.StackRegistry) (username, password, server string) {
	server = registryFromImage(imageRef)

	// Explicit per-stack credentials win over the legacy single-cred
	// fallback.  This means an operator who has migrated to the new
	// API can still leave their old fields populated without it
	// silently overriding.
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if registryHostsMatch(c.Server, server) {
			return c.Username, c.Password, server
		}
	}
	if legacy != nil && legacy.Username != "" && registryHostsMatch(legacy.Server, server) {
		return legacy.Username, legacy.Password, server
	}
	return "", "", server
}

// registryHostsMatch is a forgiving comparison that treats the various
// Docker Hub aliases as equivalent. Hub credentials saved under
// "docker.io" should match images with "index.docker.io/..." prefixes
// and vice versa, so operators don't need to know which alias their
// CI / `docker login` produced.
func registryHostsMatch(a, b string) bool {
	a = canonicalRegistryHost(a)
	b = canonicalRegistryHost(b)
	return a == b
}

func canonicalRegistryHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return defaultRegistryHost
	}
	return host
}
