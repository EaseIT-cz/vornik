package hostdoctor

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/imagemanifest"
	"vornik.io/vornik/internal/registry"
)

// checkPodmanConfig verifies podman runtime configuration.
func (h *Checker) checkPodmanConfig(ctx context.Context) Check {
	name := "podman_config"

	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		return Check{Name: name, Status: "ERROR", Message: "podman not found in PATH"}
	}

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, podmanPath, "info", "--format",
		"{{.Host.RemoteSocket.Exists}} {{.Store.GraphRoot}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return Check{
			Name:    name,
			Status:  "ERROR",
			Message: "podman info failed",
			Items:   []string{strings.TrimSpace(string(output))},
		}
	}

	var items []string

	// Check subuid/subgid for rootless operation
	cmd = exec.CommandContext(checkCtx, podmanPath, "info", "--format", "{{.Host.IDMappings.UIDMap}}")
	uidOut, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(uidOut)) == "[]" {
		items = append(items, "WARNING: no UID mappings — rootless containers may fail (check /etc/subuid)")
	}

	if len(items) > 0 {
		return Check{Name: name, Status: "WARNING", Message: "podman available with warnings", Items: items}
	}
	return Check{Name: name, Status: "OK", Message: fmt.Sprintf("podman OK (%s)", podmanPath)}
}

// agentImagesFromSwarms collects the unique, real (non-empty, non-"noop:")
// agent images referenced by any role across swarms. Shared by
// checkAgentImages and firstAgentImage so both walk the same set of images
// the same way. Skips the "noop:" sentinel prefix used for non-containerised
// roles like the dispatcher — runtime.image is required by the registry
// loader, but those roles never launch a container, so podman image exists
// (or a baked-uid probe) would always falsely flag them.
func agentImagesFromSwarms(swarms map[string]*registry.Swarm) map[string]bool {
	images := make(map[string]bool)
	for _, swarm := range swarms {
		for _, role := range swarm.Roles {
			if role.Runtime.Image == "" || strings.HasPrefix(role.Runtime.Image, "noop:") {
				continue
			}
			images[role.Runtime.Image] = true
		}
	}
	return images
}

// firstAgentImage returns one representative real agent image configured
// under configDir, or "" if none are configured. checkAgentImageUID only
// needs a single agent image to compare its baked uid against the host uid
// (unlike checkAgentImages, which must check every image's local
// availability), so this picks the lexicographically-first image name for
// determinism rather than returning the whole set.
func firstAgentImage(configDir string) (string, error) {
	swarms, err := registry.LoadSwarms(configDir)
	if err != nil {
		return "", err
	}
	images := agentImagesFromSwarms(swarms)
	if len(images) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(images))
	for image := range images {
		names = append(names, image)
	}
	sort.Strings(names)
	return names[0], nil
}

// checkAgentImages verifies that agent images referenced in swarm configs are available locally.
func (h *Checker) checkAgentImages(ctx context.Context) Check {
	name := "agent_images"

	if h.configDir == "" {
		return Check{Name: name, Status: "SKIPPED", Message: "no config directory configured, skipping image check"}
	}

	swarms, err := registry.LoadSwarms(h.configDir)
	if err != nil {
		return Check{Name: name, Status: "ERROR", Message: fmt.Sprintf("failed to load swarms: %v", err)}
	}

	// Collect unique images. Skip the "noop:" sentinel prefix used for
	// non-containerised roles like the dispatcher — runtime.image is
	// required by the registry loader, but those roles never launch a
	// container, so podman image exists would always falsely flag them.
	images := agentImagesFromSwarms(swarms)

	if len(images) == 0 {
		return Check{Name: name, Status: "OK", Message: "no agent images configured"}
	}

	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		return Check{Name: name, Status: "WARNING", Message: "podman not found, cannot verify images"}
	}

	var missing []string
	for image := range images {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(checkCtx, podmanPath, "image", "exists", image)
		if err := cmd.Run(); err != nil {
			missing = append(missing, image)
		}
		cancel()
	}

	if len(missing) > 0 {
		return classifyMissingAgentImages(missing)
	}

	return Check{Name: name, Status: "OK", Message: fmt.Sprintf("all %d agent images available", len(images))}
}

// classifyMissingAgentImages rates the refs `podman image exists` did not find
// (image-freshness design §18). Split by whether the missing reference can
// actually be pulled:
//
//   - an unqualified short name cannot: under `short-name-mode = enforced`
//     (the host default) podman must prompt for a registry and, with no TTY,
//     the run fails outright — the 2026-06-27 incident, where the
//     swarmd→vornik rename left configs pointing at the unbuilt short name
//     `swarmd-agent:latest`;
//   - a localhost/ ref cannot either: localhost/ is podman's namespace for
//     images built or tagged on this host and no registry serves it (a pull
//     dials https://localhost/v2/ and is refused). Rated a WARNING until
//     2026-10-12, which is how a missing localhost/vornik-agent:bench failed a
//     whole bench chunk at container start (GitHub #309);
//   - a registry-qualified ref (host with '.' or ':', or a digest under one)
//     is fetched on first use, so it stays a WARNING.
//
// Either unpullable kind is an ERROR listing every unpullable ref, with the
// remedy for each kind present.
func classifyMissingAgentImages(missing []string) Check {
	const name = "agent_images"
	var short, local []string
	for _, m := range missing {
		switch {
		case imageIsUnqualified(m):
			short = append(short, m)
		case imageIsLocalOnly(m):
			local = append(local, m)
		}
	}
	if len(short)+len(local) > 0 {
		var parts []string
		if len(short) > 0 {
			parts = append(parts, fmt.Sprintf("%d agent image(s) missing AND unqualified — podman cannot resolve a short name without a TTY (short-name resolution enforced), so every job using these swarms will fail at container start. Build/tag the image locally or qualify the reference with a registry.", len(short)))
		}
		if len(local) > 0 {
			parts = append(parts, fmt.Sprintf("%d agent image(s) under localhost/ are missing — no registry serves localhost/, so podman cannot pull them and every job using these swarms will fail at container start. Build or tag them on this host: %s", len(local), localImageRemedy(local)))
		}
		blocking := append(append([]string{}, short...), local...)
		sort.Strings(blocking)
		return Check{Name: name, Status: "ERROR", Message: strings.Join(parts, " "), Items: blocking}
	}
	sorted := append([]string{}, missing...)
	sort.Strings(sorted)
	return Check{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d agent images not found locally (will be pulled on first use)", len(sorted)),
		Items:   sorted,
	}
}

// localImageRemedy names the commands that put a missing localhost/ image on
// this host. For the agent image the build tag is known, so the exact pair is
// printed; for anything else the generic build-or-tag form.
func localImageRemedy(local []string) string {
	generic := "`make install-images` builds every manifest image at its manifest tag; then `podman tag <built image> <ref>` for a ref the manifest does not name."
	for _, ref := range local {
		if strings.HasPrefix(ref, "localhost/vornik-agent:") || ref == "localhost/vornik-agent" {
			return fmt.Sprintf("`make build-agent && podman tag %s %s` (the agent image builds as %s). Otherwise %s",
				imagemanifest.AgentImageTag, ref, imagemanifest.AgentImageTag, generic)
		}
	}
	return generic
}

// imageIsLocalOnly reports whether ref lives in podman's localhost/ namespace:
// its first path segment is exactly "localhost". "localhost:5000/x" is a real
// registry on a port and is NOT local-only.
func imageIsLocalOnly(ref string) bool {
	return strings.HasPrefix(ref, "localhost/")
}

// imageIsUnqualified reports whether ref is a container "short name" — an
// image reference with no registry component (e.g. "vornik-agent:latest"
// or "library/ubuntu"). Such names rely on unqualified-search-registries
// plus an interactive prompt to resolve; under `short-name-mode =
// enforced` with no TTY they cannot be pulled at all. A reference whose
// first path segment looks like a registry host (contains '.' or ':') or
// is the special "localhost" is qualified and remains pullable.
func imageIsUnqualified(ref string) bool {
	slash := strings.IndexByte(ref, '/')
	if slash < 0 {
		return true
	}
	first := ref[:slash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return false
	}
	return true
}
