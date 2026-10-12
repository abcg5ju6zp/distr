package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os/signal"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/buildconfig"
	"github.com/distr-sh/distr/internal/controllerapply"
	"github.com/distr-sh/distr/internal/controllerauth"
	"github.com/distr-sh/distr/internal/controllercheck"
	"github.com/distr-sh/distr/internal/controllerclient"
	"github.com/distr-sh/distr/internal/controllerenv"
	"github.com/distr-sh/distr/internal/controllerlogging"
	"github.com/distr-sh/distr/internal/deploymenttargetlogs"
	"github.com/distr-sh/distr/internal/types"
	"github.com/distr-sh/distr/internal/util"
	dockercommand "github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	platformLoggingCore = &deploymenttargetlogs.Core{Encoder: zapcore.NewConsoleEncoder(func() zapcore.EncoderConfig {
		cfg := zap.NewDevelopmentEncoderConfig()
		cfg.TimeKey = ""
		cfg.LevelKey = ""
		return cfg
	}())}
	logger = util.Require(zap.NewDevelopment(
		zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			// Platform logging should use the same logging level as the base core
			platformLoggingCore.LevelEnabler = c
			return zapcore.NewTee(c, platformLoggingCore)
		}),
	))
	client         = util.Require(controllerclient.NewFromEnv(logger))
	dockerCli      = util.Require(dockercommand.NewDockerCli())
	composeService composeapi.Compose
	health         = controllercheck.NewServer(time.Hour)
	logWatcher     = NewLogsWatcher(30 * time.Second)
	logCollector   = &deploymenttargetlogs.BufferedCollector{}
)

func init() {
	logCollector.Delegate = client
	platformLoggingCore.Collector = logCollector
	controllerlogging.Redirect(logger)
	if controllerenv.ControllerVersionID == "" {
		logger.Warn("DISTR_CONTROLLER_VERSION_ID is not set. self updates will be disabled")
	}
	util.Must(dockerCli.Initialize(flags.NewClientOptions()))
	composeService = util.Require(compose.NewComposeService(dockerCli))
}

func main() {
	defer func() {
		if err := logger.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
			fmt.Println(err)
		}
	}()

	defer func() {
		if err := logCollector.Stop(); err != nil {
			fmt.Println(err)
		}
	}()

	defer func() {
		if reason := recover(); reason != nil {
			logger.Panic("controller panic", zap.Any("reason", reason))
		}
	}()

	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)

	context.AfterFunc(ctx, func() { logger.Info("shutdown signal received") })

	logger.Info("docker controller is starting",
		zap.String("version", buildconfig.Version()),
		zap.String("commit", buildconfig.Commit()),
		zap.Bool("release", buildconfig.IsRelease()))

	if err := deploymentStore.SweepIncomplete(); err != nil {
		logger.Warn("could not sweep incomplete deployment state", zap.Error(err))
	}

	go func() {
		if err := startHealthServer(); err != nil {
			logger.Warn("health server error", zap.Error(err))
		}
	}()

	mainLoop(ctx)

	logger.Info("shutting down")
}

func mainLoop(ctx context.Context) {
	tick := time.Tick(controllerenv.Interval)
	logsGoroutine := util.NewToggleableGoroutine(logWatcher.Watch)
	deploymentMetricsGoroutine := util.NewToggleableGoroutine(watchDeploymentMetrics)
	imageDiskUsageGoroutine := util.NewToggleableGoroutine(watchImageDiskUsage)

	go watchStatus(ctx)

loop:
	for ctx.Err() == nil {
		select {
		case <-tick:
		case <-ctx.Done():
			break loop
		}

		health.Heartbeat()

		if resource, err := client.Resource(ctx); err != nil {
			logger.Error("failed to get resource", zap.Error(err))
		} else {
			publishTargetRevisionIDs(resource.Deployments)

			if selfUpdateIfRequired(ctx, *resource) {
				continue
			}

			logWatcher.SetLogsAfter(resource.DeploymentLogsAfter)
			logsGoroutine.GoOrCancel(ctx, resource.DeploymentLogsEnabled)

			if resource.MetricsEnabled {
				startMetrics(ctx)
			} else {
				stopMetrics(ctx)
			}
			deploymentMetricsGoroutine.GoOrCancel(ctx, resource.MetricsEnabled)
			imageDiskUsageGoroutine.GoOrCancel(ctx, resource.MetricsEnabled)

			deployments, corruptions, err := GetExistingDeployments()
			if err != nil {
				logger.Error("could not get existing deployments", zap.Error(err))
			} else {
				reportCorruptions(ctx, *resource, corruptions)
				cleanupOldDeployments(ctx, *resource, slices.Collect(maps.Values(deployments)))
			}

			reconcilePendingReports(ctx, *resource)

			if len(resource.Deployments) == 0 {
				logger.Info("no deployment in resource response")
				continue
			}

			for _, deployment := range resource.Deployments {
				applyDeployment(ctx, deployment, deployments)
			}
		}
	}
}

func applyDeployment(
	ctx context.Context,
	deployment api.ControllerDeployment,
	existing map[uuid.UUID]ControllerDeployment,
) {
	if deployment.DockerType == nil {
		logger.Error("cannot apply deployment because docker type is nil",
			zap.Any("deploymentRevisionId", deployment.RevisionID))
		return
	}

	var current *ControllerDeployment
	if d, ok := existing[deployment.ID]; ok {
		current = &d
	}

	if !requiresApply(deployment.RevisionID, current) {
		if *deployment.DockerType == types.DockerTypeCompose {
			if err := EnsureComposeProjectDir(ctx, deployment); err != nil {
				logger.Warn("could not write compose project directory", zap.Error(err))
			}
		}
		return
	}

	if _, err := controllerauth.EnsureAuth(ctx, client.RawToken(), deployment); err != nil {
		logger.Error("docker auth error", zap.Error(err))
		sendApplyStatus(ctx, deployment, err)
		return
	}

	// Only one controller process or goroutine applies a deployment at a time. A self-update can run a
	// second controller while the old one is finishing, so the lock is a file lock, not an in-process one.
	unlock, locked, err := deploymentStore.TryLock(deployment.ID)
	if err != nil {
		logger.Error("could not acquire deployment lock",
			zap.Stringer("deploymentId", deployment.ID), zap.Error(err))
		sendApplyStatus(ctx, deployment, err)
		return
	}
	if !locked {
		logger.Info("deployment is being applied by another controller process, skipping",
			zap.Stringer("deploymentId", deployment.ID))
		return
	}
	defer unlock()

	projectName, err := getProjectName(deployment.ComposeFile)
	if err != nil {
		logger.Error("invalid compose file", zap.Error(err))
		sendApplyStatus(ctx, deployment, err)
		return
	}

	generation, adopted, corruptions, err := deploymentStore.Claim(controllerapply.Target{
		DeploymentID: deployment.ID,
		RevisionID:   deployment.RevisionID,
		ProjectName:  projectName,
		DockerType:   *deployment.DockerType,
	})
	if err != nil {
		logger.Error("could not claim application generation", zap.Error(err))
		sendApplyStatus(ctx, deployment, err)
		return
	}
	for _, corruption := range corruptions {
		logger.Error("corrupt generation record quarantined",
			zap.Stringer("deploymentId", deployment.ID),
			zap.String("path", corruption.Path),
			zap.String("quarantined", corruption.Quarantined),
			zap.Error(corruption.Err))
	}
	logger.Info("applying deployment revision",
		zap.Stringer("deploymentId", deployment.ID),
		zap.Stringer("generationId", generation.ID),
		zap.Bool("adoptedGeneration", adopted))

	// A finalized generation means the engine result is settled. The on-disk state is repaired from the
	// receipt if an earlier crash left it progressing, and only the server callback remains outstanding.
	if generation.Phase == controllerapply.PhaseFinalized {
		reconcileFinalizedGeneration(ctx, generation, current, deployment)
		return
	}

	var previousDeploymentImages []string
	if current != nil {
		if images, err := GetDeploymentImages(ctx, *current); err != nil {
			logger.Error("failed to get old images", zap.Error(err))
		} else {
			previousDeploymentImages = images
		}
	}

	updateStatus, stopProgress := sendProgressInterval(ctx, deployment)
	appliedDeployment, receipt, status, applyErr := ApplyGeneration(
		ctx, generation, deployment, current, updateStatus)

	var restartErr error
	if applyErr == nil && appliedDeployment != nil && deployment.ForceRestart &&
		!generationHasRestart(generation) {
		var restartReceipt controllerapply.Receipt
		restartReceipt, restartErr = restartWithReceipt(ctx, *appliedDeployment)
		generation.Actions = append(generation.Actions, restartReceipt)
	}
	stopProgress()

	if appliedDeployment == nil {
		sendApplyStatus(ctx, deployment, applyErr)
		return
	}

	// The engine may or may not have applied the revision. The generation stays claimed and the next
	// tick reconciles from the receipt and the actual resources; it is never marked failed or re-created.
	if errors.Is(applyErr, errOutcomeUnknown) {
		generation.Receipt = receipt
		if saveErr := deploymentStore.SaveGeneration(generation); saveErr != nil {
			logger.Warn("failed to persist uncertain apply receipt", zap.Error(saveErr))
		}
		logger.Warn("container engine action result is unknown; continuing the same generation on the next tick",
			zap.Stringer("generationId", generation.ID), zap.Error(applyErr))
		return
	}

	published := finalizeGeneration(ctx, generation, appliedDeployment, receipt, status, applyErr, restartErr, deployment)

	if published && applyErr == nil && deployment.ImageCleanupEnabled {
		currentImages := receiptImages(receipt)
		currentImages = append(currentImages, specServiceImages(deployment.ComposeFile)...)
		if delErr := DeleteImages(ctx,
			controllerapply.ObsoleteImages(previousDeploymentImages, currentImages)); delErr != nil {
			logger.Warn("failed to delete old images", zap.Error(delErr))
		}
	}
}

// finalizeGeneration persists the engine receipt, publishes the final state and delivers the final
// status from the receipt. A newer generation prevents every one of these writes and reports false so
// the caller skips follow-up work, notably image cleanup, for the superseded revision.
func finalizeGeneration(
	ctx context.Context,
	generation *controllerapply.Generation,
	appliedDeployment *ControllerDeployment,
	receipt *controllerapply.Receipt,
	status string,
	applyErr, restartErr error,
	deployment api.ControllerDeployment,
) bool {
	reportErr := errors.Join(applyErr, restartErr)
	if receipt == nil {
		receipt = &controllerapply.Receipt{StartedAt: receiptNow()}
	}
	if reportErr != nil {
		receipt.Outcome = controllerapply.OutcomeFailed
		receipt.ReportType = types.DeploymentStatusTypeError
		receipt.ReportMessage = reportErr.Error()
	} else {
		if receipt.Outcome == controllerapply.OutcomeUnspecified {
			receipt.Outcome = controllerapply.OutcomeSucceeded
		}
		receipt.ReportType = types.DeploymentStatusTypeProgressing
		receipt.ReportMessage = status
	}
	receipt.FinishedAt = receiptNow()
	generation.Receipt = receipt
	generation.Phase = controllerapply.PhaseFinalized

	if err := deploymentStore.SaveGeneration(generation); err != nil {
		// The receipt is the restart recovery record: if it was not finalized, the next tick adopts the
		// still-claimed generation and reconciles without touching the engine again.
		logger.Warn("failed to persist application generation receipt",
			zap.Stringer("generationId", generation.ID), zap.Error(err))
		return false
	}

	finalDeployment := *appliedDeployment
	if applyErr == nil {
		finalDeployment.State = StateReady
		finalDeployment.CurrentRevisionID = finalDeployment.RevisionID
	} else {
		finalDeployment.State = StateFailed
	}

	// The durable state is published first, so a callback failure can never turn a successful apply into
	// a retry: the receipt drives the callback until the server confirms it.
	if err := deploymentStore.PublishState(finalDeployment); err != nil {
		if errors.Is(err, controllerapply.ErrGenerationSuperseded) {
			return false
		}
		logger.Warn("failed to save deployment after apply",
			zap.Stringer("deploymentId", finalDeployment.ID), zap.Error(err))
	}

	reportGenerationReceipt(ctx, generation, deployment)
	_ = deploymentStore.PruneGenerations(deployment.ID)
	return true
}

// reconcileFinalizedGeneration finishes a generation whose engine result is already durable: it repairs
// a state file a crash left progressing or failed, then re-delivers the final callback from the receipt.
// The engine is never called again for this generation.
func reconcileFinalizedGeneration(
	ctx context.Context,
	generation *controllerapply.Generation,
	current *ControllerDeployment,
	deployment api.ControllerDeployment,
) {
	finalDeployment := ControllerDeployment{
		ID:           generation.DeploymentID,
		RevisionID:   generation.RevisionID,
		GenerationID: generation.ID,
		ProjectName:  generation.ProjectName,
		DockerType:   generation.DockerType,
	}
	if current != nil && current.ID == generation.DeploymentID {
		finalDeployment.CurrentRevisionID = current.AppliedRevisionID()
	}
	if generation.Receipt != nil && generation.Receipt.Outcome == controllerapply.OutcomeSucceeded {
		finalDeployment.State = StateReady
		finalDeployment.CurrentRevisionID = generation.RevisionID
	} else {
		finalDeployment.State = StateFailed
	}
	if err := deploymentStore.PublishState(finalDeployment); err != nil {
		if !errors.Is(err, controllerapply.ErrGenerationSuperseded) {
			logger.Warn("failed to repair deployment state from generation receipt",
				zap.Stringer("generationId", generation.ID), zap.Error(err))
		}
		return
	}

	reportGenerationReceipt(ctx, generation, deployment)
	_ = deploymentStore.PruneGenerations(deployment.ID)
}

// reportGenerationReceipt delivers the final status stored in the generation receipt and marks it
// reported. A failed delivery leaves the receipt untouched for the next reconciliation.
func reportGenerationReceipt(
	ctx context.Context,
	generation *controllerapply.Generation,
	deployment api.ControllerDeployment,
) bool {
	if generation.Receipt == nil {
		return true
	}
	deployment.RevisionID = generation.RevisionID
	if err := client.Status(ctx, deployment,
		generation.Receipt.ReportType, generation.Receipt.ReportMessage); err != nil {
		logger.Warn("failed to send final status from receipt",
			zap.Stringer("generationId", generation.ID), zap.Error(err))
		return false
	}
	generation.Receipt.Reported = true
	if err := deploymentStore.SaveGeneration(generation); err != nil {
		logger.Warn("failed to mark generation receipt as reported", zap.Error(err))
	}
	return true
}

// reconcilePendingReports re-delivers final statuses that never reached the server, including after a
// process restart, from their persisted receipts. A success is never downgraded: only the stored receipt
// is replayed.
func reconcilePendingReports(ctx context.Context, resource api.ControllerResource) {
	pending, err := deploymentStore.PendingGenerations()
	if err != nil {
		logger.Warn("could not load pending generation receipts", zap.Error(err))
		return
	}
	for _, generation := range pending {
		deployment, ok := resourceDeployment(resource.Deployments, generation.DeploymentID)
		if !ok {
			continue
		}
		// A newer target revision or a newer generation owns the status now, so the stale receipt is
		// abandoned rather than reported against the new revision.
		if deployment.RevisionID != generation.RevisionID || !isNewestGeneration(generation) {
			generation.Receipt.Reported = true
			if err := deploymentStore.SaveGeneration(generation); err != nil {
				logger.Warn("failed to abandon stale generation receipt", zap.Error(err))
			}
			continue
		}
		if reportGenerationReceipt(ctx, generation, deployment) {
			_ = deploymentStore.PruneGenerations(generation.DeploymentID)
		}
	}
}

func isNewestGeneration(generation *controllerapply.Generation) bool {
	gens, _, err := deploymentStore.LoadGenerations(generation.DeploymentID)
	if err != nil || len(gens) == 0 {
		return false
	}
	newest := gens[0]
	for _, g := range gens[1:] {
		if g.ClaimedAt.After(newest.ClaimedAt) {
			newest = g
		}
	}
	return newest.ID == generation.ID
}

func resourceDeployment(deployments []api.ControllerDeployment, id uuid.UUID) (api.ControllerDeployment, bool) {
	for _, deployment := range deployments {
		if deployment.ID == id {
			return deployment, true
		}
	}
	return api.ControllerDeployment{}, false
}

func generationHasRestart(generation *controllerapply.Generation) bool {
	for _, action := range generation.Actions {
		if action.Action == controllerapply.ActionComposeRestart ||
			action.Action == controllerapply.ActionSwarmRestart {
			return true
		}
	}
	return false
}

func receiptImages(receipt *controllerapply.Receipt) []string {
	if receipt == nil {
		return nil
	}
	images := make([]string, 0, len(receipt.Resources))
	for _, resource := range receipt.Resources {
		if resource.Image != "" {
			images = append(images, resource.Image)
		}
	}
	return images
}

// reportCorruptions reports quarantined state files as deployment errors without blocking the remaining
// deployments. A deployment missing its state is simply applied again on this tick.
func reportCorruptions(ctx context.Context, resource api.ControllerResource, corruptions []controllerapply.Corruption) {
	for _, corruption := range corruptions {
		logger.Error("corrupt deployment state quarantined",
			zap.Stringer("deploymentId", corruption.DeploymentID),
			zap.String("path", corruption.Path),
			zap.String("quarantined", corruption.Quarantined),
			zap.NamedError("cause", corruption.Err))
		if deployment, ok := resourceDeployment(resource.Deployments, corruption.DeploymentID); ok {
			_ = client.StatusWithError(ctx, deployment,
				fmt.Errorf("local deployment state was corrupt and has been quarantined: %w", corruption.Err))
		}
	}
}

func requiresApply(targetRevisionID uuid.UUID, current *ControllerDeployment) bool {
	return current == nil ||
		current.RevisionID != targetRevisionID ||
		current.State == StateFailed ||
		current.State == StateProgressing
}

func publishTargetRevisionIDs(deployments []api.ControllerDeployment) {
	targets := make(map[uuid.UUID]uuid.UUID, len(deployments))
	for _, deployment := range deployments {
		targets[deployment.ID] = deployment.RevisionID
	}
	targetRevisionIDs.Store(&targets)
}

func sendApplyStatus(ctx context.Context, deployment api.ControllerDeployment, err error) {
	if err := client.StatusWithError(ctx, deployment, err); err != nil {
		logger.Error("failed to send status", zap.Error(err))
	}
}

// sendProgressInterval reports progressing until the returned stop function is called, which returns only once
// no further report can be sent.
func sendProgressInterval(ctx context.Context, deployment api.ControllerDeployment) (func(string), func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var status atomic.Value
	status.Store("initializing")

	sendProgress := func() {
		err := client.Status(ctx,
			deployment, types.DeploymentStatusTypeProgressing, status.Load().(string))
		if err != nil {
			logger.Warn("error updating status", zap.Error(err))
		}
	}

	sendProgress()

	go func() {
		defer close(done)
		tick := time.Tick(controllerenv.ProgressingInterval)
		for {
			select {
			case <-ctx.Done():
				logger.Debug("stop sending progress updates")
				return
			case <-tick:
				logger.Info("sending progress update")
				sendProgress()
			}
		}
	}()

	stop := func() {
		cancel()
		<-done
	}
	return func(s string) { status.Store(s) }, stop
}

func startHealthServer() error {
	err := http.ListenAndServe("127.0.0.1:8765", health)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func selfUpdateIfRequired(ctx context.Context, resource api.ControllerResource) bool {
	if controllerenv.ControllerVersionID != "" {
		if controllerenv.ControllerVersionID != resource.Version.ID.String() {
			logger.Info("controller version has changed. starting self-update")
			if err := RunControllerSelfUpdate(ctx); err != nil {
				logger.Error("self update failed", zap.Error(err))
				// TODO: Support status without revision ID?
				if len(resource.Deployments) > 0 {
					if err := client.StatusWithError(ctx, resource.Deployments[0], err); err != nil {
						logger.Error("failed to send status", zap.Error(err))
					}
				}
			} else {
				logger.Info("self-update has been applied")
				return true
			}
		} else {
			logger.Debug("controller version is up to date")
		}
	}
	return false
}

func cleanupOldDeployments(ctx context.Context, resource api.ControllerResource, deployments []ControllerDeployment) {
	for _, deployment := range deployments {
		resourceHasExistingDeployment := slices.ContainsFunc(
			resource.Deployments,
			func(d api.ControllerDeployment) bool { return d.ID == deployment.ID },
		)
		if resourceHasExistingDeployment {
			continue
		}

		func() {
			unlock, locked, err := deploymentStore.TryLock(deployment.ID)
			if err != nil {
				logger.Error("could not acquire lock for old deployment cleanup",
					zap.Stringer("deploymentId", deployment.ID), zap.Error(err))
				return
			}
			if !locked {
				logger.Info("old deployment is locked by another controller process, skipping cleanup",
					zap.Stringer("deploymentId", deployment.ID))
				return
			}
			defer unlock()

			// The compose project name comes from the deployment's own compose file and two different
			// deployments can declare the same name. Removing the old one must never touch a project a
			// current target still declares.
			if projectNameStillTargeted(deployment.ProjectName, resource.Deployments) {
				logger.Warn("not uninstalling old deployment: its project name is still declared by a current target",
					zap.Stringer("deploymentId", deployment.ID), zap.String("projectName", deployment.ProjectName))
				return
			}

			logger.Info("uninstalling old deployment", zap.String("id", deployment.ID.String()))

			deploymentImages, err := GetDeploymentImages(ctx, deployment)
			if err != nil {
				logger.Error("could not get images for old deployment", zap.Error(err))
			}

			if err := DockerEngineUninstall(ctx, deployment); err != nil {
				logger.Warn("could not uninstall deployment", zap.Error(err))
			} else if err := DeleteImages(ctx, deploymentImages); err != nil {
				logger.Warn("could not delete images for old deployment", zap.Error(err))
			}

			if err := DeleteDeployment(deployment); err != nil {
				logger.Warn("could not delete deployment", zap.Error(err))
			}
			if err := deploymentStore.DeleteGenerations(deployment.ID); err != nil {
				logger.Warn("could not delete deployment generations", zap.Error(err))
			}
			if err := DeleteComposeProjectDir(deployment.ID); err != nil {
				logger.Warn("could not delete compose project directory", zap.Error(err))
			}
			if err := deploymentStore.RemoveLockFile(deployment.ID); err != nil {
				logger.Warn("could not remove deployment lock file", zap.Error(err))
			}

			logWatcher.CleanupLogsTimestamps(deployment)
		}()
	}
}

func projectNameStillTargeted(projectName string, targets []api.ControllerDeployment) bool {
	if projectName == "" {
		return false
	}
	return slices.ContainsFunc(targets, func(t api.ControllerDeployment) bool {
		if t.DockerType == nil {
			return false
		}
		name, err := getProjectName(t.ComposeFile)
		return err == nil && name == projectName
	})
}
