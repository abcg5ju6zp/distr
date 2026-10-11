package types

import (
	"slices"
	"time"

	"github.com/google/uuid"
)

type OverviewDeployment struct {
	DeploymentWithLatestRevision
	// CreatedByName and CreatedByImageID are only set when the revision was created by a member of the
	// customer organization, since a customer must not learn who works for the vendor.
	CreatedByName    *string    `db:"created_by_name"`
	CreatedByImageID *uuid.UUID `db:"created_by_image_id"`
}

type OverviewArtifactPull struct {
	ArtifactID           uuid.UUID  `db:"artifact_id"`
	VersionName          string     `db:"version_name"`
	ManifestBlobDigest   Digest     `db:"manifest_blob_digest"`
	CreatedAt            time.Time  `db:"created_at"`
	UserName             *string    `db:"user_name"`
	UserImageID          *uuid.UUID `db:"user_image_id"`
	DeploymentTargetName *string    `db:"deployment_target_name"`
}

type OverviewChecklist struct {
	ProfileCompleted   bool `db:"profile_completed"`
	TeamMembersInvited bool `db:"team_members_invited"`
	DeploymentCreated  bool `db:"deployment_created"`
	SecretCreated      bool `db:"secret_created"`
	AccessTokenCreated bool `db:"access_token_created"`
	ArtifactPulled     bool `db:"artifact_pulled"`
}

// OverviewLicense is the license key or entitlement that decides the state of a customer's license.
type OverviewLicense struct {
	Name string `db:"name"`
	// NotBefore is only set for a license key, since an entitlement is valid from its creation.
	NotBefore *time.Time `db:"not_before"`
	ExpiresAt *time.Time `db:"expires_at"`
}

type OverviewAlertSummary struct {
	AlertConfigurationCount int64 `db:"alert_configuration_count"`
	NotificationCount       int64 `db:"notification_count"`
}

type OverviewActivityType string

const (
	OverviewActivityTypeApplicationVersionCreated  OverviewActivityType = "application_version_created"
	OverviewActivityTypeArtifactVersionCreated     OverviewActivityType = "artifact_version_created"
	OverviewActivityTypeArtifactVersionFirstPulled OverviewActivityType = "artifact_version_first_pulled"
	OverviewActivityTypeDeploymentRevisionCreated  OverviewActivityType = "deployment_revision_created"
	OverviewActivityTypeLicenseKeyCreated          OverviewActivityType = "license_key_created"
	OverviewActivityTypeLicenseKeyUpdated          OverviewActivityType = "license_key_updated"
	OverviewActivityTypeSupportBundleCreated       OverviewActivityType = "support_bundle_created"
	OverviewActivityTypeSupportBundleStatusChanged OverviewActivityType = "support_bundle_status_changed"
)

type OverviewActivity struct {
	ID          uuid.UUID            `db:"id"`
	CreatedAt   time.Time            `db:"created_at"`
	Type        OverviewActivityType `db:"type"`
	Subject     string               `db:"subject"`
	Detail      *string              `db:"detail"`
	UserName    *string              `db:"user_name"`
	UserImageID *uuid.UUID           `db:"user_image_id"`
}

// LatestEntitledApplicationVersion returns the newest version of the application that the entitlement
// covers, or of the whole application when there is no entitlement or it covers every version. The
// ordering is derived from all versions of the application, see ApplicationVersionComparator.
func LatestEntitledApplicationVersion(
	application Application,
	entitlement *ApplicationEntitlementWithVersions,
) *ApplicationVersion {
	compare := ApplicationVersionComparator(application.VersioningStrategy, application.Versions)
	candidates := application.Versions
	if entitlement != nil && len(entitlement.Versions) > 0 {
		candidates = slices.DeleteFunc(slices.Clone(application.Versions), func(v ApplicationVersion) bool {
			return !entitlement.HasVersionWithID(v.ID)
		})
	}
	return LatestApplicationVersion(compare, candidates)
}

// ApplicationUpdateAvailable reports whether latest is newer than the version with currentVersionID.
func ApplicationUpdateAvailable(application Application, currentVersionID uuid.UUID, latest *ApplicationVersion) bool {
	if latest == nil || latest.ID == currentVersionID {
		return false
	}
	index := slices.IndexFunc(application.Versions, func(v ApplicationVersion) bool { return v.ID == currentVersionID })
	if index < 0 {
		return true
	}
	compare := ApplicationVersionComparator(application.VersioningStrategy, application.Versions)
	return compare(*latest, application.Versions[index]) > 0
}

// NextExpiresAt returns the next expiration among the credentials of the token that still authenticate,
// or nil when none of them expires. Once every credential has expired, it returns the last expiration.
// A credential that expired while another one still authenticates is ignored as a completed rotation.
func (tok AccessToken) NextExpiresAt(now time.Time) *time.Time {
	var expirations []*time.Time
	if tok.KeyIsCredential {
		expirations = append(expirations, tok.ExpiresAt)
	}
	for _, slot := range AccessTokenSecretSlots {
		if secret := tok.Secret(slot); secret != nil {
			expirations = append(expirations, secret.ExpiresAt)
		}
	}
	var next, lastExpired *time.Time
	valid := false
	for _, expiresAt := range expirations {
		switch {
		case expiresAt == nil:
			valid = true
		case expiresAt.After(now):
			valid = true
			if next == nil || expiresAt.Before(*next) {
				next = expiresAt
			}
		case lastExpired == nil || expiresAt.After(*lastExpired):
			lastExpired = expiresAt
		}
	}
	if valid {
		return next
	}
	return lastExpired
}
