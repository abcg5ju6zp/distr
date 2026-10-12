package supportbundle

import "github.com/google/uuid"

// Stable keys of the required manifest items fixed at bundle creation. The collect script reports
// every resource under one of these keys (or under a self-registered optional key for resources
// discovered at runtime, such as per-container logs), which is what makes retried uploads
// idempotent and lets finalize tell a complete collection from a partial one.
const (
	SystemInfoStableKey = "system-info"
	HostEnvStableKey    = "host-environment-variables"

	// CustomScriptStableKeyPrefix names the manifest resource of one snapshotted custom script.
	CustomScriptStableKeyPrefix = "custom-script:"
)

// CustomScriptStableKey returns the stable resource key of the custom script with the given
// configuration script id.
func CustomScriptStableKey(configurationScriptID uuid.UUID) string {
	return CustomScriptStableKeyPrefix + configurationScriptID.String()
}
