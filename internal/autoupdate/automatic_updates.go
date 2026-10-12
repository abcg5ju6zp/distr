// Package autoupdate runs automatic application upgrades as persistent batches.
//
// A batch fixes the version ordering, the entitlements and the deployment set of one upgrade and
// records one result per deployment. Each deployment target loads its secrets and license keys
// independently, each deployment is validated and revised in its own transaction, and a failure
// of one target or deployment only ever records that item's result: revisions already created
// stay in place and the triggering request commits the partial success. Unconfirmed items are
// claimed by retry sweeps that also run at startup, and an item never overwrites a revision a
// user or a newer batch created after its snapshot.
package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/apierrors"
	internalctx "github.com/distr-sh/distr/internal/context"
	"github.com/distr-sh/distr/internal/db"
	"github.com/distr-sh/distr/internal/deploymentvalues"
	"github.com/distr-sh/distr/internal/types"
	"github.com/distr-sh/distr/internal/util"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Machine-readable item reasons, stored in AutomaticUpdateBatchItem.Message.
const (
	reasonAutoUpdatesDisabled      = "automatic_updates_disabled"
	reasonTargetUnavailable        = "target_unavailable"
	reasonTargetGone               = "target_gone"
	reasonSecretsUnavailable       = "secrets_unavailable"
	reasonLicenseKeysUnavailable   = "license_keys_unavailable"
	reasonDeploymentGone           = "deployment_gone"
	reasonDeploymentUnavailable    = "deployment_unavailable"
	reasonEntitlementUnavailable   = "entitlement_unavailable"
	reasonNotEntitled              = "not_entitled"
	reasonTargetVersionUnavailable = "target_version_unavailable"
	reasonTargetArchived           = "target_archived"
	reasonLatestRevisionMissing    = "latest_revision_missing"
	reasonAlreadyOnTarget          = "already_on_target"
	reasonSuperseded               = "superseded"
	reasonValuesIncompatible       = "values_incompatible"
	reasonValuesRenderFailed       = "values_render_failed"
	reasonRevisionCreateFailed     = "revision_create_failed"
)

// maxBatchesPerSweep bounds the work one startup or cron sweep takes.
const maxBatchesPerSweep = 100

// Sentinel errors tagging which stage of a target bundle failed, so every item gets the precise
// retryable reason.
var (
	errSecretsUnavailable     = errors.New("secrets unavailable")
	errLicenseKeysUnavailable = errors.New("license keys unavailable")
)

// Allowed reports whether the organization and the application currently permit automatic
// updates. A deployment can outlive all three conditions, since the vendor may withdraw them from
// the application and the organization may lose the feature with its plan.
func Allowed(org *types.Organization, application *types.Application) bool {
	return org.HasFeature(types.FeatureAutoUpdates) &&
		application.AllowAutomaticUpdates &&
		application.VersioningStrategy.AllowsAutomaticUpdates()
}

// Trigger starts the automatic upgrade for an application (or for one deployment when
// onlyDeploymentID is set). It has to be called inside the triggering transaction, after the
// triggering change has been written. It only returns an error when the batch itself cannot be
// persisted; per-deployment failures are recorded on the batch and never roll the trigger back.
func Trigger(
	ctx context.Context,
	org *types.Organization,
	applicationID uuid.UUID,
	onlyDeploymentID *uuid.UUID,
	createdByUserID *uuid.UUID,
) error {
	// The advisory lock serializes batch creation with version, application and entitlement
	// changes, so two triggers never snapshot each other's missing rows.
	if err := db.LockApplicationExclusive(ctx, applicationID); err != nil {
		return err
	}

	application, err := db.GetApplication(ctx, applicationID, org.ID)
	if err != nil {
		return err
	}
	if !Allowed(org, application) {
		// Automatic updates were turned off (or the feature withdrawn): batches from before the
		// change stop instead of creating more revisions.
		return db.StopAutomaticUpdateBatchesForApplication(ctx, applicationID, reasonAutoUpdatesDisabled)
	}

	deployments, err := db.GetDeploymentsWithAutomaticApplicationUpdates(ctx, applicationID)
	if err != nil {
		return err
	}
	if onlyDeploymentID != nil {
		deployments = slices.DeleteFunc(deployments, func(d types.DeploymentWithLatestRevision) bool {
			return d.ID != *onlyDeploymentID
		})
	}
	if len(deployments) == 0 {
		return nil
	}

	// The ordering is taken from all of the application's versions rather than from the ones a
	// single deployment is entitled to, so that every deployment of the application agrees on it.
	compare := types.ApplicationVersionComparator(application.VersioningStrategy, application.Versions)
	orderedVersions := slices.Clone(application.Versions)
	types.SortApplicationVersions(application.VersioningStrategy, orderedVersions)
	versionIDs := make([]uuid.UUID, len(orderedVersions))
	for i, version := range orderedVersions {
		versionIDs[i] = version.ID
	}

	log := internalctx.GetLogger(ctx)
	items := make([]types.AutomaticUpdateBatchItem, 0, len(deployments))
	for _, deployment := range deployments {
		candidates, err := entitledApplicationVersions(ctx, application, deployment)
		if err != nil {
			return err
		}
		latest := types.LatestApplicationVersion(compare, candidates)
		if latest == nil {
			continue
		}
		currentIndex := slices.IndexFunc(application.Versions, func(v types.ApplicationVersion) bool {
			return v.ID == deployment.ApplicationVersionID
		})
		if currentIndex < 0 {
			log.Warn("skipping automatic update because the deployed version does not belong to the application",
				zap.String("deploymentId", deployment.ID.String()))
			continue
		}
		if compare(*latest, application.Versions[currentIndex]) <= 0 {
			continue
		}

		currentRevisionID := deployment.DeploymentRevisionID
		currentVersionID := deployment.ApplicationVersionID
		targetVersionID := latest.ID
		items = append(items, types.AutomaticUpdateBatchItem{
			DeploymentID:             deployment.ID,
			DeploymentTargetID:       deployment.DeploymentTargetID,
			CurrentRevisionID:        &currentRevisionID,
			CurrentVersionID:         &currentVersionID,
			TargetVersionID:          &targetVersionID,
			ApplicationEntitlementID: deployment.ApplicationEntitlementID,
		})
	}

	// Nothing to do: keep the historical no-op behavior and leave no empty batch behind.
	if len(items) == 0 {
		return nil
	}

	batch := &types.AutomaticUpdateBatch{
		OrganizationID:           org.ID,
		ApplicationID:            applicationID,
		TriggeredByUserAccountID: createdByUserID,
		OnlyDeploymentID:         onlyDeploymentID,
		VersioningStrategy:       application.VersioningStrategy,
		VersionIDs:               versionIDs,
	}
	if err := db.CreateAutomaticUpdateBatch(ctx, batch, items); err != nil {
		return err
	}

	return processBatch(ctx, application, batch)
}

// RunPendingBatches claims every batch with unconfirmed items and processes it. Each application
// is locked and handled in its own transaction, so it serializes against live triggers and other
// sweeps. A batch that fails to load leaves an error in the log and stays for the next sweep.
func RunPendingBatches(ctx context.Context) error {
	log := internalctx.GetLogger(ctx)
	batches, err := db.GetRunningAutomaticUpdateBatches(ctx, maxBatchesPerSweep)
	if err != nil {
		return err
	}
	for _, batch := range batches {
		if err := runPendingBatch(ctx, batch); err != nil {
			log.Warn("could not process automatic update batch",
				zap.String("batchId", batch.ID.String()), zap.Error(err))
		}
	}
	return nil
}

func runPendingBatch(ctx context.Context, batch types.AutomaticUpdateBatch) error {
	return db.RunTx(ctx, func(ctx context.Context) error {
		if err := db.LockApplicationExclusive(ctx, batch.ApplicationID); err != nil {
			return err
		}
		org, err := db.GetOrganizationByID(ctx, batch.OrganizationID)
		if err != nil {
			return err
		}
		application, err := db.GetApplication(ctx, batch.ApplicationID, batch.OrganizationID)
		if errors.Is(err, apierrors.ErrNotFound) {
			// The application is gone and the batch went with it.
			return nil
		} else if err != nil {
			return err
		}
		if !Allowed(org, application) {
			return db.StopAutomaticUpdateBatchesForApplication(ctx, application.ID, reasonAutoUpdatesDisabled)
		}
		return processBatch(ctx, application, &batch)
	})
}

// processBatch works through the open items of a batch. It runs inside the batch's transaction;
// every target load and every item runs in a nested savepoint transaction, so one failure never
// rolls back another deployment's result.
func processBatch(
	ctx context.Context,
	application *types.Application,
	batch *types.AutomaticUpdateBatch,
) error {
	items, err := db.GetOpenAutomaticUpdateBatchItems(ctx, batch.ID)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	log := internalctx.GetLogger(ctx).With(zap.String("automaticUpdateBatchId", batch.ID.String()))

	byTarget := make(map[uuid.UUID][]types.AutomaticUpdateBatchItem)
	for _, item := range items {
		byTarget[item.DeploymentTargetID] = append(byTarget[item.DeploymentTargetID], item)
	}
	compare := snapshotVersionComparator(batch.VersionIDs)

	for targetID, itemsForTarget := range byTarget {
		target, secrets, licenseKeys, err := loadTargetBundle(ctx, targetID)
		if err != nil {
			status, reason := classifyTargetLoadError(err)
			for _, item := range itemsForTarget {
				recordItem(ctx, log, item.ID, status, nil, reason)
			}
			continue
		}

		for _, item := range itemsForTarget {
			status, revisionID, reason := processItem(
				ctx, application, batch, target, secrets, licenseKeys, item, compare,
			)
			recordItem(ctx, log, item.ID, status, revisionID, reason)
		}
	}

	summary, err := db.GetAutomaticUpdateBatchByID(ctx, batch.ID)
	if err != nil {
		return err
	}
	log.Info("automatic update batch finished",
		zap.Int("total", summary.TotalCount),
		zap.Int("upgraded", summary.UpgradedCount),
		zap.Int("skipped", summary.SkippedCount),
		zap.Int("incompatible", summary.IncompatibleCount),
		zap.Int("retryable", summary.RetryableCount))
	return nil
}

// loadTargetBundle loads one target and the secrets and license keys of all its deployments, in a
// savepoint: a failure rolls only that load back and leaves every other target untouched.
func loadTargetBundle(
	ctx context.Context,
	targetID uuid.UUID,
) (*types.DeploymentTargetFull, []types.SecretWithUpdatedBy, []types.LicenseKey, error) {
	var target *types.DeploymentTargetFull
	var secrets []types.SecretWithUpdatedBy
	var licenseKeys []types.LicenseKey
	err := db.RunTx(ctx, func(ctx context.Context) error {
		var err error
		target, err = db.GetDeploymentTarget(ctx, targetID, nil, nil)
		if err != nil {
			return err
		}
		secrets, err = db.GetSecretsForDeploymentTarget(ctx, target.DeploymentTarget)
		if err != nil {
			return fmt.Errorf("%w: %w", errSecretsUnavailable, err)
		}
		licenseKeys, err = db.GetLicenseKeysForDeploymentTarget(ctx, target.DeploymentTarget)
		if err != nil {
			return fmt.Errorf("%w: %w", errLicenseKeysUnavailable, err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return target, secrets, licenseKeys, nil
}

func classifyTargetLoadError(err error) (types.AutomaticUpdateBatchItemStatus, string) {
	if errors.Is(err, apierrors.ErrNotFound) {
		return types.AutomaticUpdateBatchItemSkipped, reasonTargetGone
	}
	// The three loads can only fail together here; the reason says which stage the message from
	// the savepoint came from.
	switch {
	case errors.Is(err, errLicenseKeysUnavailable):
		return types.AutomaticUpdateBatchItemRetryable, reasonLicenseKeysUnavailable
	case errors.Is(err, errSecretsUnavailable):
		return types.AutomaticUpdateBatchItemRetryable, reasonSecretsUnavailable
	default:
		return types.AutomaticUpdateBatchItemRetryable, reasonTargetUnavailable
	}
}

// processItem decides and applies one item inside a savepoint. The returned status and reason are
// only persisted by the caller, so an unexpected error rolls the item's writes back cleanly.
func processItem(
	ctx context.Context,
	application *types.Application,
	batch *types.AutomaticUpdateBatch,
	target *types.DeploymentTargetFull,
	secrets []types.SecretWithUpdatedBy,
	licenseKeys []types.LicenseKey,
	item types.AutomaticUpdateBatchItem,
	compare func(a, b uuid.UUID) int,
) (types.AutomaticUpdateBatchItemStatus, *uuid.UUID, string) {
	var status types.AutomaticUpdateBatchItemStatus
	var revisionID *uuid.UUID
	var reason string
	// The decision and the revision insert run in a savepoint of their own, so rolling the item
	// back never discards results of the other deployments. The result is persisted separately
	// afterwards; a lost result is recovered by adopting the item-linked revision on the next run.
	_ = db.RunTx(ctx, func(ctx context.Context) error {
		status, revisionID, reason = decideItem(ctx, application, batch, target, secrets, licenseKeys, item, compare)
		return nil
	})
	if status == "" {
		// The savepoint failed before any decision could be recorded (e.g. the connection broke):
		// leave a retryable result for the next sweep.
		status = types.AutomaticUpdateBatchItemRetryable
		reason = reasonDeploymentUnavailable
	}
	return status, revisionID, reason
}

func recordItem(
	ctx context.Context,
	log *zap.Logger,
	itemID uuid.UUID,
	status types.AutomaticUpdateBatchItemStatus,
	revisionID *uuid.UUID,
	reason string,
) {
	err := db.RunTx(ctx, func(ctx context.Context) error {
		return db.MarkAutomaticUpdateBatchItem(ctx, itemID, status, revisionID, &reason)
	})
	if err != nil {
		log.Warn("could not record automatic update item result",
			zap.String("automaticUpdateBatchItemId", itemID.String()), zap.Error(err))
	}
}

func decideItem(
	ctx context.Context,
	application *types.Application,
	batch *types.AutomaticUpdateBatch,
	target *types.DeploymentTargetFull,
	secrets []types.SecretWithUpdatedBy,
	licenseKeys []types.LicenseKey,
	item types.AutomaticUpdateBatchItem,
	compare func(a, b uuid.UUID) int,
) (types.AutomaticUpdateBatchItemStatus, *uuid.UUID, string) {
	itemLog := internalctx.GetLogger(ctx).With(zap.String("deploymentId", item.DeploymentID.String()))

	// Lock the deployment row: this is the arbitration point against manual revisions, which do
	// not take the application advisory lock.
	state, err := db.GetDeploymentLatestStateForUpdate(ctx, item.DeploymentID)
	if errors.Is(err, apierrors.ErrNotFound) {
		return types.AutomaticUpdateBatchItemSkipped, nil, reasonDeploymentGone
	} else if err != nil {
		return types.AutomaticUpdateBatchItemRetryable, nil, reasonDeploymentUnavailable
	}

	// A previous attempt already created this item's revision (e.g. its response was lost): the
	// upgrade is a fact, so adopt the revision instead of inserting a duplicate. This has to run
	// before every later gate, none of which can undo the revision.
	existing, err := db.GetAutomaticUpdateRevisionForBatchItem(ctx, item.ID)
	if err != nil {
		return types.AutomaticUpdateBatchItemRetryable, nil, reasonRevisionCreateFailed
	}
	if existing != nil {
		return types.AutomaticUpdateBatchItemUpgraded, existing, ""
	}

	if !state.AutomaticApplicationUpdatesEnabled {
		return types.AutomaticUpdateBatchItemSkipped, nil, reasonAutoUpdatesDisabled
	}

	if item.TargetVersionID == nil || item.CurrentRevisionID == nil {
		return types.AutomaticUpdateBatchItemSkipped, nil, reasonTargetVersionUnavailable
	}
	targetVersionID := *item.TargetVersionID
	targetVersion := slices.IndexFunc(application.Versions, func(v types.ApplicationVersion) bool {
		return v.ID == targetVersionID
	})
	if targetVersion < 0 {
		return types.AutomaticUpdateBatchItemSkipped, nil, reasonTargetVersionUnavailable
	}
	if application.Versions[targetVersion].ArchivedAt != nil {
		return types.AutomaticUpdateBatchItemSkipped, nil, reasonTargetArchived
	}

	// Re-check the entitlement as it is now, from the deployment's current entitlement: an
	// entitlement narrowed after the snapshot stops the item rather than forcing a version the
	// customer is no longer entitled to.
	entitled, err := deploymentEntitledToVersion(ctx, state.ApplicationEntitlementID, targetVersionID)
	if err != nil {
		return types.AutomaticUpdateBatchItemRetryable, nil, reasonEntitlementUnavailable
	}
	if !entitled {
		return types.AutomaticUpdateBatchItemSkipped, nil, reasonNotEntitled
	}

	if state.LatestDeploymentRevisionID == nil {
		return types.AutomaticUpdateBatchItemRetryable, nil, reasonLatestRevisionMissing
	}

	// The deployment configuration to roll forward starts from the revision in the snapshot. A
	// revision appearing after the snapshot only wins when it is an automatic revision still
	// behind the target; a manual or newer revision stops the item.
	deploymentConfigIndex := slices.IndexFunc(target.Deployments,
		func(d types.DeploymentWithLatestRevision) bool { return d.ID == item.DeploymentID })
	if deploymentConfigIndex < 0 {
		return types.AutomaticUpdateBatchItemSkipped, nil, reasonDeploymentGone
	}
	deployment := target.Deployments[deploymentConfigIndex]

	if *state.LatestDeploymentRevisionID != *item.CurrentRevisionID {
		if state.LatestApplicationVersionID == nil {
			return types.AutomaticUpdateBatchItemRetryable, nil, reasonLatestRevisionMissing
		}
		switch latestVersionID := *state.LatestApplicationVersionID; {
		case latestVersionID == targetVersionID:
			// Another batch, a manual deployment or an affected-deployment rollout already moved
			// the deployment to the target version.
			return types.AutomaticUpdateBatchItemSkipped, nil, reasonAlreadyOnTarget
		case compare(latestVersionID, targetVersionID) > 0:
			// The deployment is ahead of the fixed target; the old snapshot must not downgrade it.
			return types.AutomaticUpdateBatchItemSkipped, nil, reasonSuperseded
		case state.LatestRevisionTrigger == nil ||
			*state.LatestRevisionTrigger != types.DeploymentRevisionTriggerAutomaticUpdate:
			// A newer manual/secret/license revision interleaves with the batch: never overwrite it.
			return types.AutomaticUpdateBatchItemSkipped, nil, reasonSuperseded
		}

		// An older automatic batch moved the deployment to a version still below the target.
		// Reload that configuration under the lock and roll it forward instead of using the
		// snapshot copy.
		reloaded, err := db.GetDeploymentsForDeploymentTarget(ctx, item.DeploymentTargetID)
		if err != nil {
			return types.AutomaticUpdateBatchItemRetryable, nil, reasonDeploymentUnavailable
		}
		index := slices.IndexFunc(reloaded, func(d types.DeploymentWithLatestRevision) bool {
			return d.ID == item.DeploymentID
		})
		if index < 0 {
			return types.AutomaticUpdateBatchItemSkipped, nil, reasonDeploymentGone
		}
		deployment = reloaded[index]
	}

	latestWithFiles, err := db.GetApplicationVersion(ctx, targetVersionID)
	if err != nil {
		return types.AutomaticUpdateBatchItemRetryable, nil, reasonTargetVersionUnavailable
	}

	request := deploymentRequestFromLatestRevision(deployment)
	request.ApplicationVersionID = targetVersionID
	request.CreatedByUserAccountID = batch.TriggeredByUserAccountID
	request.Trigger = types.DeploymentRevisionTriggerAutomaticUpdate

	if err := validateAutomaticUpdateValues(&request, latestWithFiles, secrets, licenseKeys); err != nil {
		itemLog.Warn("automatic update stopped because the deployment values do not apply to the new version",
			zap.String("applicationVersionId", targetVersionID.String()), zap.Error(err))
		return types.AutomaticUpdateBatchItemIncompatible, nil, reasonValuesIncompatible
	}
	if err := setDeploymentRequestValuesHash(&request, secrets, licenseKeys); err != nil {
		itemLog.Warn("automatic update stopped because the deployment values could not be rendered",
			zap.Error(err))
		return types.AutomaticUpdateBatchItemIncompatible, nil, reasonValuesRenderFailed
	}

	// The insert runs in a savepoint of its own: a failed write leaves Postgres in an aborted
	// subtransaction state, which the adoption lookup below needs rolled back first.
	var revision *types.DeploymentRevision
	if err := db.RunTx(ctx, func(ctx context.Context) error {
		var err error
		revision, err = db.CreateAutomaticUpdateDeploymentRevision(ctx, &request, item.ID)
		return err
	}); err != nil {
		// The response is uncertain: look the revision up before declaring the item retryable,
		// rather than inserting a second one.
		existing, lookupErr := db.GetAutomaticUpdateRevisionForBatchItem(ctx, item.ID)
		if lookupErr == nil && existing != nil {
			return types.AutomaticUpdateBatchItemUpgraded, existing, ""
		}
		itemLog.Warn("could not create automatic update revision", zap.Error(err))
		return types.AutomaticUpdateBatchItemRetryable, nil, reasonRevisionCreateFailed
	}

	itemLog.Info("deployment updated automatically",
		zap.String("applicationVersionId", targetVersionID.String()),
		zap.String("applicationVersionName", latestWithFiles.Name))
	return types.AutomaticUpdateBatchItemUpgraded, &revision.ID, ""
}

// entitledApplicationVersions are the versions the deployment may be updated to at batch
// creation. An entitlement without versions entitles the customer to all of them.
func entitledApplicationVersions(
	ctx context.Context,
	application *types.Application,
	deployment types.DeploymentWithLatestRevision,
) ([]types.ApplicationVersion, error) {
	if deployment.ApplicationEntitlementID == nil {
		return application.Versions, nil
	}
	entitlement, err := db.GetApplicationEntitlementByID(ctx, *deployment.ApplicationEntitlementID)
	if err != nil {
		return nil, err
	}
	if len(entitlement.Versions) == 0 {
		return application.Versions, nil
	}
	return entitlement.Versions, nil
}

// deploymentEntitledToVersion reports whether the deployment's current entitlement covers the
// target version.
func deploymentEntitledToVersion(
	ctx context.Context,
	entitlementID *uuid.UUID,
	versionID uuid.UUID,
) (bool, error) {
	if entitlementID == nil {
		return true, nil
	}
	entitlement, err := db.GetApplicationEntitlementByID(ctx, *entitlementID)
	if errors.Is(err, apierrors.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if len(entitlement.Versions) == 0 {
		return true, nil
	}
	return slices.ContainsFunc(entitlement.Versions, func(v types.ApplicationVersion) bool {
		return v.ID == versionID
	}), nil
}

func validateAutomaticUpdateValues(
	request *api.DeploymentRequest,
	applicationVersion *types.ApplicationVersion,
	secrets []types.SecretWithUpdatedBy,
	licenseKeys []types.LicenseKey,
) error {
	deploymentValues, err := deploymentvalues.ParsedValuesFileReplaceSecrets(request, secrets, licenseKeys)
	if err != nil {
		return err
	}
	applicationVersionValues, err := applicationVersion.ParsedValuesFile()
	if err != nil {
		return err
	}
	if _, err := util.MergeAllRecursive(applicationVersionValues, deploymentValues); err != nil {
		return err
	}
	_, err = deploymentvalues.EnvFileReplaceSecrets(request, secrets, licenseKeys)
	return err
}

func setDeploymentRequestValuesHash(
	request *api.DeploymentRequest,
	secrets []types.SecretWithUpdatedBy,
	licenseKeys []types.LicenseKey,
) error {
	hash, err := deploymentvalues.RenderAndHash(request, secrets, licenseKeys)
	if err != nil {
		return err
	}
	request.ValuesHash = hash[:]
	return nil
}

func deploymentRequestFromLatestRevision(deployment types.DeploymentWithLatestRevision) api.DeploymentRequest {
	return api.DeploymentRequest{
		DeploymentID:             &deployment.ID,
		DeploymentTargetID:       deployment.DeploymentTargetID,
		ApplicationVersionID:     deployment.ApplicationVersionID,
		ApplicationEntitlementID: deployment.ApplicationEntitlementID,
		ReleaseName:              deployment.ReleaseName,
		ValuesYaml:               deployment.ValuesYaml,
		DockerType:               deployment.DockerType,
		EnvFileData:              deployment.EnvFileData,
		HelmOptions:              apiHelmOptionsFromInternal(deployment.HelmOptions),
	}
}

func apiHelmOptionsFromInternal(options *types.HelmOptions) *api.HelmOptions {
	if options == nil {
		return nil
	}
	return &api.HelmOptions{
		Timeout:           options.Timeout,
		WaitStrategy:      options.WaitStrategy,
		RollbackOnFailure: options.RollbackOnFailure,
		CleanupOnFailure:  options.CleanupOnFailure,
		ForceConflicts:    options.ForceConflicts,
	}
}

// snapshotVersionComparator fixes the version ordering of a batch: versions compare by their
// position in the snapshotted ASC order, and a version published afterwards compares newer than
// every snapshotted one.
func snapshotVersionComparator(versionIDs []uuid.UUID) func(a, b uuid.UUID) int {
	position := make(map[uuid.UUID]int, len(versionIDs))
	for i, id := range versionIDs {
		position[id] = i
	}
	return func(a, b uuid.UUID) int {
		pa, okA := position[a]
		pb, okB := position[b]
		if !okA {
			pa = len(versionIDs)
		}
		if !okB {
			pb = len(versionIDs)
		}
		switch {
		case pa < pb:
			return -1
		case pa > pb:
			return 1
		case a == b:
			return 0
		case a.String() < b.String():
			return -1
		default:
			return 1
		}
	}
}
