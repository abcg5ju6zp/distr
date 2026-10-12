package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/distr-sh/distr/internal/apierrors"
	internalctx "github.com/distr-sh/distr/internal/context"
	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreateAutomaticUpdateBatch persists a batch together with its fixed deployment set. The items
// carry the decision snapshot (current/target versions, entitlement); the batch carries the fixed
// version ordering. It has to run inside the triggering transaction.
func CreateAutomaticUpdateBatch(
	ctx context.Context,
	batch *types.AutomaticUpdateBatch,
	items []types.AutomaticUpdateBatchItem,
) error {
	db := internalctx.GetDb(ctx)
	row := db.QueryRow(ctx,
		`INSERT INTO AutomaticUpdateBatch (
			organization_id, application_id, triggered_by_user_account_id, only_deployment_id,
			versioning_strategy, version_ids, total_count
		) VALUES (
			@organizationId, @applicationId, @triggeredBy, @onlyDeploymentId,
			@versioningStrategy, @versionIds, @totalCount
		) RETURNING id, created_at`,
		pgx.NamedArgs{
			"organizationId":     batch.OrganizationID,
			"applicationId":      batch.ApplicationID,
			"triggeredBy":        batch.TriggeredByUserAccountID,
			"onlyDeploymentId":   batch.OnlyDeploymentID,
			"versioningStrategy": batch.VersioningStrategy,
			"versionIds":         batch.VersionIDs,
			"totalCount":         len(items),
		})
	if err := row.Scan(&batch.ID, &batch.CreatedAt); err != nil {
		return fmt.Errorf("could not save AutomaticUpdateBatch: %w", err)
	}
	batch.TotalCount = len(items)

	deploymentIDs := make([]uuid.UUID, len(items))
	targetIDs := make([]uuid.UUID, len(items))
	currentRevisionIDs := make([]*uuid.UUID, len(items))
	currentVersionIDs := make([]*uuid.UUID, len(items))
	targetVersionIDs := make([]*uuid.UUID, len(items))
	entitlementIDs := make([]*uuid.UUID, len(items))
	for i, item := range items {
		deploymentIDs[i] = item.DeploymentID
		targetIDs[i] = item.DeploymentTargetID
		currentRevisionIDs[i] = item.CurrentRevisionID
		currentVersionIDs[i] = item.CurrentVersionID
		targetVersionIDs[i] = item.TargetVersionID
		entitlementIDs[i] = item.ApplicationEntitlementID
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO AutomaticUpdateBatchItem (
			batch_id, deployment_id, deployment_target_id, current_revision_id, current_version_id,
			target_version_id, application_entitlement_id
		)
		SELECT @batchId, x.deployment_id, x.deployment_target_id, x.current_revision_id,
			x.current_version_id, x.target_version_id, x.application_entitlement_id
		FROM unnest(
			@deploymentIds::uuid[], @deploymentTargetIds::uuid[], @currentRevisionIds::uuid[],
			@currentVersionIds::uuid[], @targetVersionIds::uuid[], @entitlementIds::uuid[]
		) AS x(deployment_id, deployment_target_id, current_revision_id, current_version_id,
			target_version_id, application_entitlement_id)`,
		pgx.NamedArgs{
			"batchId":             batch.ID,
			"deploymentIds":       deploymentIDs,
			"deploymentTargetIds": targetIDs,
			"currentRevisionIds":  currentRevisionIDs,
			"currentVersionIds":   currentVersionIDs,
			"targetVersionIds":    targetVersionIDs,
			"entitlementIds":      entitlementIDs,
		}); err != nil {
		return fmt.Errorf("could not save AutomaticUpdateBatchItems: %w", err)
	}
	return nil
}

// GetRunningAutomaticUpdateBatches returns batches with still-unconfirmed items, oldest first.
func GetRunningAutomaticUpdateBatches(
	ctx context.Context,
	limit int,
) ([]types.AutomaticUpdateBatch, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`SELECT id, created_at, completed_at, organization_id, application_id,
			triggered_by_user_account_id, only_deployment_id, versioning_strategy, version_ids,
			total_count, upgraded_count, skipped_count, incompatible_count, retryable_count
		FROM AutomaticUpdateBatch
		WHERE completed_at IS NULL
		ORDER BY created_at
		LIMIT @limit`,
		pgx.NamedArgs{"limit": limit})
	if err != nil {
		return nil, fmt.Errorf("failed to query AutomaticUpdateBatches: %w", err)
	}
	batches, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.AutomaticUpdateBatch])
	if err != nil {
		return nil, fmt.Errorf("failed to scan AutomaticUpdateBatches: %w", err)
	}
	return batches, nil
}

// GetAutomaticUpdateBatchByID returns the batch with the given ID.
func GetAutomaticUpdateBatchByID(
	ctx context.Context,
	id uuid.UUID,
) (*types.AutomaticUpdateBatch, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`SELECT id, created_at, completed_at, organization_id, application_id,
			triggered_by_user_account_id, only_deployment_id, versioning_strategy, version_ids,
			total_count, upgraded_count, skipped_count, incompatible_count, retryable_count
		FROM AutomaticUpdateBatch
		WHERE id = @id`,
		pgx.NamedArgs{"id": id})
	if err != nil {
		return nil, fmt.Errorf("failed to query AutomaticUpdateBatch: %w", err)
	}
	batch, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.AutomaticUpdateBatch])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierrors.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan AutomaticUpdateBatch: %w", err)
	}
	return &batch, nil
}

// GetOpenAutomaticUpdateBatchItems returns the unconfirmed items (pending or retryable) of a
// batch, in the deterministic order they were snapshotted in.
func GetOpenAutomaticUpdateBatchItems(
	ctx context.Context,
	batchID uuid.UUID,
) ([]types.AutomaticUpdateBatchItem, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`SELECT id, created_at, updated_at, batch_id, deployment_id, deployment_target_id, status,
			current_revision_id, current_version_id, target_version_id, application_entitlement_id,
			revision_id, attempts, message
		FROM AutomaticUpdateBatchItem
		WHERE batch_id = @batchId AND status IN ('pending', 'retryable')
		ORDER BY created_at`,
		pgx.NamedArgs{"batchId": batchID})
	if err != nil {
		return nil, fmt.Errorf("failed to query AutomaticUpdateBatchItems: %w", err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.AutomaticUpdateBatchItem])
	if err != nil {
		return nil, fmt.Errorf("failed to scan AutomaticUpdateBatchItems: %w", err)
	}
	return items, nil
}

// MarkAutomaticUpdateBatchItem records one item's result, counts the attempt and recomputes the
// batch summary, completing the batch once no unconfirmed items remain.
func MarkAutomaticUpdateBatchItem(
	ctx context.Context,
	itemID uuid.UUID,
	status types.AutomaticUpdateBatchItemStatus,
	revisionID *uuid.UUID,
	message *string,
) error {
	db := internalctx.GetDb(ctx)
	var batchID uuid.UUID
	if err := db.QueryRow(ctx,
		`UPDATE AutomaticUpdateBatchItem
		SET status = @status::AUTOMATIC_UPDATE_BATCH_ITEM_STATUS,
			revision_id = COALESCE(@revisionId, revision_id),
			updated_at = now(),
			attempts = attempts + 1,
			message = @message
		WHERE id = @id
		RETURNING batch_id`,
		pgx.NamedArgs{
			"id":         itemID,
			"status":     string(status),
			"revisionId": revisionID,
			"message":    message,
		}).Scan(&batchID); err != nil {
		return fmt.Errorf("could not update AutomaticUpdateBatchItem: %w", err)
	}
	if err := recomputeAutomaticUpdateBatch(ctx, batchID); err != nil {
		return err
	}
	return nil
}

// recomputeAutomaticUpdateBatch refreshes the counters of one batch and marks it completed once
// it has no pending or retryable items left.
func recomputeAutomaticUpdateBatch(ctx context.Context, batchID uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	_, err := db.Exec(ctx,
		`UPDATE AutomaticUpdateBatch b
		SET total_count = c.total_count,
			upgraded_count = c.upgraded_count,
			skipped_count = c.skipped_count,
			incompatible_count = c.incompatible_count,
			retryable_count = c.retryable_count,
			completed_at = CASE
				WHEN c.upgraded_count + c.skipped_count + c.incompatible_count = c.total_count
				THEN now() ELSE b.completed_at END
		FROM (
			SELECT batch_id,
				count(*) AS total_count,
				count(*) FILTER (WHERE status = 'upgraded') AS upgraded_count,
				count(*) FILTER (WHERE status = 'skipped') AS skipped_count,
				count(*) FILTER (WHERE status = 'incompatible') AS incompatible_count,
				count(*) FILTER (WHERE status = 'retryable') AS retryable_count
			FROM AutomaticUpdateBatchItem
			WHERE batch_id = @batchId
			GROUP BY batch_id
		) c
		WHERE b.id = c.batch_id`,
		pgx.NamedArgs{"batchId": batchID})
	if err != nil {
		return fmt.Errorf("could not recompute AutomaticUpdateBatch: %w", err)
	}
	return nil
}

// StopAutomaticUpdateBatchesForApplication finishes every still-open batch of an application by
// recording its unconfirmed items as skipped. It is used when the application or organization no
// longer allows automatic updates: an old batch then stops instead of creating more revisions.
func StopAutomaticUpdateBatchesForApplication(
	ctx context.Context,
	applicationID uuid.UUID,
	reason string,
) error {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`UPDATE AutomaticUpdateBatchItem i
		SET status = 'skipped', updated_at = now(), message = @reason
		FROM AutomaticUpdateBatch b
		WHERE i.batch_id = b.id
			AND b.application_id = @applicationId
			AND b.completed_at IS NULL
			AND i.status IN ('pending', 'retryable')
		RETURNING i.batch_id`,
		pgx.NamedArgs{"applicationId": applicationID, "reason": reason})
	if err != nil {
		return fmt.Errorf("could not stop AutomaticUpdateBatches: %w", err)
	}
	batchIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("could not scan stopped AutomaticUpdateBatches: %w", err)
	}
	seen := make(map[uuid.UUID]struct{}, len(batchIDs))
	for _, batchID := range batchIDs {
		if _, ok := seen[batchID]; ok {
			continue
		}
		seen[batchID] = struct{}{}
		if err := recomputeAutomaticUpdateBatch(ctx, batchID); err != nil {
			return err
		}
	}
	return nil
}

// GetAutomaticUpdateRevisionForBatchItem returns the ID of the revision already created for a
// batch item, or nil when none exists. It adopts the revision when an insert's response was lost.
func GetAutomaticUpdateRevisionForBatchItem(
	ctx context.Context,
	batchItemID uuid.UUID,
) (*uuid.UUID, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`SELECT id
		FROM DeploymentRevision
		WHERE automatic_update_batch_item_id = @batchItemId`,
		pgx.NamedArgs{"batchItemId": batchItemID})
	if err != nil {
		return nil, fmt.Errorf("failed to query DeploymentRevision: %w", err)
	}
	revisionID, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[uuid.UUID])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan DeploymentRevision: %w", err)
	}
	return &revisionID, nil
}

// DeploymentLatestState is the current state of a deployment row, locked for the duration of the
// surrounding transaction while a batch item decides on it.
type DeploymentLatestState struct {
	AutomaticApplicationUpdatesEnabled bool                             `db:"automatic_application_updates_enabled"`
	ApplicationEntitlementID           *uuid.UUID                       `db:"application_entitlement_id"`
	LatestDeploymentRevisionID         *uuid.UUID                       `db:"latest_deployment_revision_id"`
	LatestRevisionTrigger              *types.DeploymentRevisionTrigger `db:"latest_revision_trigger"`
	LatestApplicationVersionID         *uuid.UUID                       `db:"latest_application_version_id"`
	LatestRevisionBatchItemID          *uuid.UUID                       `db:"latest_revision_batch_item_id"`
}

// GetDeploymentLatestStateForUpdate locks the deployment row and returns the state a batch item
// has to arbitrate against.
func GetDeploymentLatestStateForUpdate(
	ctx context.Context,
	deploymentID uuid.UUID,
) (*DeploymentLatestState, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`SELECT d.automatic_application_updates_enabled,
			d.application_entitlement_id,
			d.latest_deployment_revision_id,
			dr.trigger AS latest_revision_trigger,
			dr.application_version_id AS latest_application_version_id,
			dr.automatic_update_batch_item_id AS latest_revision_batch_item_id
		FROM Deployment d
			LEFT JOIN DeploymentRevision dr ON dr.id = d.latest_deployment_revision_id
		WHERE d.id = @deploymentId
		FOR UPDATE OF d`,
		pgx.NamedArgs{"deploymentId": deploymentID})
	if err != nil {
		return nil, fmt.Errorf("failed to query Deployment: %w", err)
	}
	state, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[DeploymentLatestState])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierrors.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan Deployment: %w", err)
	}
	return &state, nil
}
