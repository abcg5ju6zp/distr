package controllerapply

import "strings"

// CanonicalImageRef normalizes an image reference for comparison between a compose specification and a
// running resource: the resolved "@sha256:..." digest is dropped and a missing tag becomes "latest".
// References stay case-insensitive as on the registry.
func CanonicalImageRef(ref string) string {
	ref = strings.TrimSpace(strings.ToLower(ref))
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if ref == "" {
		return ""
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon <= lastSlash {
		ref += ":latest"
	}
	return ref
}

// ObsoleteImages returns images from previous that are not referenced by any image in current,
// comparing canonical forms. Images still used by the current revision must never be deleted.
func ObsoleteImages(previous, current []string) []string {
	currentSet := make(map[string]struct{}, len(current))
	for _, image := range current {
		if canonical := CanonicalImageRef(image); canonical != "" {
			currentSet[canonical] = struct{}{}
		}
	}
	var obsolete []string
	seen := map[string]struct{}{}
	for _, image := range previous {
		canonical := CanonicalImageRef(image)
		if canonical == "" {
			continue
		}
		if _, used := currentSet[canonical]; used {
			continue
		}
		if _, duplicated := seen[canonical]; duplicated {
			continue
		}
		seen[canonical] = struct{}{}
		obsolete = append(obsolete, image)
	}
	return obsolete
}
