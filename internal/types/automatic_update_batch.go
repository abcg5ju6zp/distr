package types

import (
	"time"

	"github.com/google/uuid"
)

type AutomaticUpdateBatchItemStatus string

const (
	// AutomaticUpdateBatchItemPending is a deployment the batch has not processed yet.
	AutomaticUpdateBatchItemPending AutomaticUpdateBatchItemStatus = "pending"
	// AutomaticUpdateBatchItemUpgraded means the item's single automatic-update revision was
	// created and the deployment was moved forward to its target version.
	AutomaticUpdateBatchItemUpgraded AutomaticUpdateBatchItemStatus = "upgraded"
	// AutomaticUpdateBatchItemSkipped is a deterministic result that created no revision:
	// already on the target version, no entitlement for it, automatic updates turned off, or a
	// newer revision (manual or from a newer batch) superseded the item.
	AutomaticUpdateBatchItemSkipped AutomaticUpdateBatchItemStatus = "skipped"
	// AutomaticUpdateBatchItemIncompatible means the deployment's values, secrets or license keys
	// cannot be applied to the target version; retrying without a change to them cannot help.
	AutomaticUpdateBatchItemIncompatible AutomaticUpdateBatchItemStatus = "incompatible"
	// AutomaticUpdateBatchItemRetryable is a transient failure: the target's data could not be
	// loaded or the revision write was lost. The deployment stays unconfirmed and retry sweeps may
	// claim the item.
	AutomaticUpdateBatchItemRetryable AutomaticUpdateBatchItemStatus = "retryable"
)

// AutomaticUpdateBatch is one automatic application upgrade. It persists the fixed snapshot the
// upgrade acts on (version ordering, triggering identity) and the summary of its per-deployment
// results, so the upgrade survives partial failures, concurrent changes and restarts.
type AutomaticUpdateBatch struct {
	Base
	CompletedAt              *time.Time         `db:"completed_at" json:"completedAt,omitempty"`
	OrganizationID           uuid.UUID          `db:"organization_id" json:"organizationId"`
	ApplicationID            uuid.UUID          `db:"application_id" json:"applicationId"`
	TriggeredByUserAccountID *uuid.UUID         `db:"triggered_by_user_account_id" json:"-"`
	OnlyDeploymentID         *uuid.UUID         `db:"only_deployment_id" json:"onlyDeploymentId,omitempty"`
	VersioningStrategy       VersioningStrategy `db:"versioning_strategy" json:"versioningStrategy"`
	VersionIDs               []uuid.UUID        `db:"version_ids" json:"versionIds"`
	TotalCount               int                `db:"total_count" json:"totalCount"`
	UpgradedCount            int                `db:"upgraded_count" json:"upgradedCount"`
	SkippedCount             int                `db:"skipped_count" json:"skippedCount"`
	IncompatibleCount        int                `db:"incompatible_count" json:"incompatibleCount"`
	RetryableCount           int                `db:"retryable_count" json:"retryableCount"`
}

// AutomaticUpdateBatchItem is one deployment's place in an AutomaticUpdateBatch, together with
// the decision snapshot and the recorded result.
type AutomaticUpdateBatchItem struct {
	Base
	UpdatedAt                time.Time                      `db:"updated_at" json:"updatedAt"`
	BatchID                  uuid.UUID                      `db:"batch_id" json:"batchId"`
	DeploymentID             uuid.UUID                      `db:"deployment_id" json:"deploymentId"`
	DeploymentTargetID       uuid.UUID                      `db:"deployment_target_id" json:"deploymentTargetId"`
	Status                   AutomaticUpdateBatchItemStatus `db:"status" json:"status"`
	CurrentRevisionID        *uuid.UUID                      `db:"current_revision_id" json:"currentRevisionId,omitempty"`
	CurrentVersionID         *uuid.UUID                      `db:"current_version_id" json:"currentVersionId,omitempty"`
	TargetVersionID          *uuid.UUID                      `db:"target_version_id" json:"targetVersionId,omitempty"`
	ApplicationEntitlementID *uuid.UUID                      `db:"application_entitlement_id" json:"applicationEntitlementId,omitempty"` //nolint:lll
	RevisionID               *uuid.UUID                      `db:"revision_id" json:"revisionId,omitempty"`
	Attempts                 int                            `db:"attempts" json:"attempts"`
	Message                  *string                        `db:"message" json:"message,omitempty"`
}

// Open reports whether the batch still has unconfirmed items a retry sweep may claim.
func (b *AutomaticUpdateBatch) Open() bool {
	return b.CompletedAt == nil
}
