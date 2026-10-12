package types

import (
	"time"

	"github.com/distr-sh/distr/internal/dbcrypto"
	"github.com/google/uuid"
)

type SupportBundleStatus string

const (
	SupportBundleStatusInitialized SupportBundleStatus = "initialized"
	SupportBundleStatusCreated     SupportBundleStatus = "created"
	SupportBundleStatusResolved    SupportBundleStatus = "resolved"
	SupportBundleStatusCanceled    SupportBundleStatus = "canceled"
)

// SupportBundleResourceStatus is the terminal state a manifest resource reached during collection.
// Every required resource must leave pending before a bundle can be finalized.
type SupportBundleResourceStatus string

const (
	SupportBundleResourceStatusPending   SupportBundleResourceStatus = "pending"
	SupportBundleResourceStatusSucceeded SupportBundleResourceStatus = "succeeded"
	SupportBundleResourceStatusSkipped   SupportBundleResourceStatus = "skipped"
	SupportBundleResourceStatusOversized SupportBundleResourceStatus = "oversized"
	SupportBundleResourceStatusFailed    SupportBundleResourceStatus = "failed"
)

// Resource report outcomes, describing what the server did with one report.
const (
	SupportBundleResourceOutcomeStored    = "stored"
	SupportBundleResourceOutcomeDuplicate = "duplicate"
	SupportBundleResourceOutcomeOversized = "oversized"
)

// SupportBundleResourceKind says where a manifest resource comes from. The fixed kinds make up the
// required manifest; container resources are discovered at runtime and register as optional items.
type SupportBundleResourceKind string

const (
	SupportBundleResourceKindSystemInfo    SupportBundleResourceKind = "system_info"
	SupportBundleResourceKindHostEnv       SupportBundleResourceKind = "host_env"
	SupportBundleResourceKindCustomScript  SupportBundleResourceKind = "custom_script"
	SupportBundleResourceKindContainerEnv  SupportBundleResourceKind = "container_env"
	SupportBundleResourceKindContainerLogs SupportBundleResourceKind = "container_logs"
	SupportBundleResourceKindOther         SupportBundleResourceKind = "other"
)

type SupportBundleConfigurationEnvVar struct {
	OrganizationID uuid.UUID `db:"organization_id"`
	Name           string    `db:"name"`
	Redacted       bool      `db:"redacted"`
}

type SupportBundleConfigurationScript struct {
	ID             uuid.UUID `db:"id"`
	CreatedAt      time.Time `db:"created_at"`
	OrganizationID uuid.UUID `db:"organization_id"`
	Name           string    `db:"name"`
	Description    *string   `db:"description"`
	Content        string    `db:"content"`
	Enabled        bool      `db:"enabled"`
}

type SupportBundle struct {
	ID                           uuid.UUID           `db:"id"`
	CreatedAt                    time.Time           `db:"created_at"`
	OrganizationID               uuid.UUID           `db:"organization_id"`
	CustomerOrganizationID       uuid.UUID           `db:"customer_organization_id"`
	CreatedByUserAccountID       *uuid.UUID          `db:"created_by_user_account_id"`
	Title                        string              `db:"title"`
	Description                  *string             `db:"description"`
	Status                       SupportBundleStatus `db:"status"`
	BundleSecret                 dbcrypto.String     `db:"bundle_secret"`
	BundleSecretExpiresAt        *time.Time          `db:"bundle_secret_expires_at"`
	StatusChangedByUserAccountID *uuid.UUID          `db:"status_changed_by_user_account_id"`
	StatusChangedAt              *time.Time          `db:"status_changed_at"`
}

type SupportBundleWithDetails struct {
	SupportBundle
	CreatedByUserName        *string    `db:"created_by_user_name"`
	CreatedByImageID         *uuid.UUID `db:"created_by_image_id"`
	CustomerOrganizationName string     `db:"customer_organization_name"`
	ResourceCount            int64      `db:"resource_count"`
	CommentCount             int64      `db:"comment_count"`
	LastCommentAt            *time.Time `db:"last_comment_at"`
	StatusChangedByUserName  *string    `db:"status_changed_by_user_name"`
	StatusChangedByImageID   *uuid.UUID `db:"status_changed_by_image_id"`
}

// SupportBundleCollection is the fixed collection generation of one bundle: the limits the collect
// script was rendered with and, once finalize committed, the published manifest outcome.
type SupportBundleCollection struct {
	SupportBundleID           uuid.UUID  `db:"support_bundle_id"`
	CollectionGeneration      uuid.UUID  `db:"collection_generation"`
	ResourceMaxBytes          int        `db:"resource_max_bytes"`
	LogTailLines              int        `db:"log_tail_lines"`
	ScriptTimeoutSeconds      int        `db:"script_timeout_seconds"`
	CreatedAt                 time.Time  `db:"created_at"`
	PublishedAt               *time.Time `db:"published_at"`
	FinalizedSucceededCount   int        `db:"finalized_succeeded_count"`
	FinalizedSkippedCount     int        `db:"finalized_skipped_count"`
	FinalizedOversizedCount   int        `db:"finalized_oversized_count"`
	FinalizedFailedCount      int        `db:"finalized_failed_count"`
}

// SupportBundleCollectionEnvVar is an immutable snapshot of one configured environment variable.
type SupportBundleCollectionEnvVar struct {
	ID              uuid.UUID `db:"id"`
	SupportBundleID uuid.UUID `db:"support_bundle_id"`
	Name            string    `db:"name"`
	Redacted        bool      `db:"redacted"`
	Position        int       `db:"position"`
}

// SupportBundleCollectionScript is an immutable snapshot of an enabled custom script. ID is the
// configuration script id and is the stable identity of the matching manifest resource.
type SupportBundleCollectionScript struct {
	ID              uuid.UUID  `db:"id"`
	SupportBundleID uuid.UUID  `db:"support_bundle_id"`
	Name            string     `db:"name"`
	Description     *string    `db:"description"`
	Content         string     `db:"content"`
	Position        int        `db:"position"`
}

type SupportBundleResource struct {
	ID              uuid.UUID                   `db:"id"`
	CreatedAt       time.Time                   `db:"created_at"`
	UpdatedAt       time.Time                   `db:"updated_at"`
	SupportBundleID uuid.UUID                   `db:"support_bundle_id"`
	// StableKey is the resource identity within the bundle. Network retries re-post the same key and
	// are idempotent; Name remains the human-readable name used in the ZIP and UI.
	StableKey string                      `db:"stable_key"`
	Name      string                      `db:"name"`
	Kind      SupportBundleResourceKind   `db:"kind"`
	Required  bool                        `db:"required"`
	Status    SupportBundleResourceStatus `db:"status"`
	// The content is a file the collect script gathered verbatim, and is served back as a download
	// and rendered in a <pre>, so it must not be trimmed if it ever reaches a JSON request body. It
	// is absent for pending/skipped/oversized outcomes.
	Content        dbcrypto.String `db:"content" trim:"-"`
	ContentSHA256  *string         `db:"content_sha256"`
	SizeBytes      int64           `db:"size_bytes"`
	FailureReason  *string         `db:"failure_reason"`
}

type SupportBundleComment struct {
	ID              uuid.UUID  `db:"id"`
	CreatedAt       time.Time  `db:"created_at"`
	SupportBundleID uuid.UUID  `db:"support_bundle_id"`
	UserAccountID   *uuid.UUID `db:"user_account_id"`
	Content         string     `db:"content"`
}

type SupportBundleCommentWithUser struct {
	SupportBundleComment
	UserName    *string    `db:"user_name"`
	UserImageID *uuid.UUID `db:"user_image_id"`
}
