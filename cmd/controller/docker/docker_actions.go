package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/controllerapply"
	"github.com/distr-sh/distr/internal/controllerauth"
	"github.com/distr-sh/distr/internal/controllerenv"
	"github.com/distr-sh/distr/internal/types"
	dockerconfig "github.com/docker/cli/cli/config"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"go.uber.org/zap"
)

const dockerApplyTimeout = 10 * time.Minute

// ApplyGeneration claims no generation itself (the caller does under the deployment lock) but drives one
// generation to an engine result and records the receipt. An ambiguous engine result is reconciled
// against the resources that actually exist, so the same generation is never applied a second time.
func ApplyGeneration(
	ctx context.Context,
	generation *controllerapply.Generation,
	deployment api.ControllerDeployment,
	previous *ControllerDeployment,
	updateStatus func(string),
) (*ControllerDeployment, *controllerapply.Receipt, string, error) {
	controllerDeployment, err := NewControllerDeployment(deployment, previous, generation.ID)
	if err != nil {
		return nil, nil, "", err
	}
	controllerDeployment.State = StateProgressing
	if err := deploymentStore.PublishState(*controllerDeployment); err != nil {
		logger.Warn("failed to save deployment before apply",
			zap.Stringer("deploymentId", controllerDeployment.ID), zap.Error(err))
	}

	applyCtx, cancel := context.WithTimeout(ctx, dockerApplyTimeout)
	defer cancel()

	var receipt *controllerapply.Receipt
	var status string
	if *deployment.DockerType == types.DockerTypeSwarm {
		logger.Debug("applying compose file in swarm mode")
		receipt, status, err = applySwarmGeneration(applyCtx, deployment, updateStatus)
	} else {
		logger.Debug("applying compose file")
		receipt, status, err = applyComposeGeneration(applyCtx, deployment, updateStatus)
	}
	return controllerDeployment, receipt, status, err
}

// applyComposeGeneration publishes the project, skips the action when the revision is already fully
// present and otherwise converges it with "compose up", recording container identities in the receipt.
func applyComposeGeneration(
	ctx context.Context,
	deployment api.ControllerDeployment,
	updateStatus func(string),
) (*controllerapply.Receipt, string, error) {
	updateStatus("initializing compose service")

	workDir, err := PublishComposeProject(ctx, deployment)
	if err != nil {
		return nil, "", err
	}

	eventProcessor := NewEventProcessor(updateStatus)
	svc, err := ComposeServiceForDeployment(deployment, compose.WithEventProcessor(eventProcessor))
	if err != nil {
		return nil, "", fmt.Errorf("failed to initialize compose service: %w", err)
	}

	project, err := svc.LoadProject(ctx, ComposeLoadOptions(workDir))
	if err != nil {
		return nil, "", fmt.Errorf("failed to load compose project: %w", err)
	}

	receipt := &controllerapply.Receipt{Action: controllerapply.ActionComposeUp, StartedAt: receiptNow()}
	if summaries := inspectComposeResources(ctx, project.Name); composeResourcesMatch(summaries, project) {
		updateStatus("resources already present")
		receipt.Outcome = controllerapply.OutcomeSucceeded
		receipt.Resources = composeResourceIdentities(summaries)
		receipt.FinishedAt = receiptNow()
		status := "compose resources already present, apply not repeated"
		receipt.Message = status
		return receipt, status, nil
	}

	err = svc.Up(ctx, project, composeapi.UpOptions{
		Create: composeapi.CreateOptions{RemoveOrphans: true},
		Start:  composeapi.StartOptions{Project: project},
	})
	summaries := inspectComposeResources(ctx, project.Name)
	receipt.Resources = composeResourceIdentities(summaries)
	receipt.FinishedAt = receiptNow()

	if err == nil {
		status := "compose command executed successfully"
		receipt.Outcome = controllerapply.OutcomeSucceeded
		receipt.Message = status
		return receipt, status, nil
	}

	if isAmbiguousEngineError(err) && composeResourcesMatch(summaries, project) {
		status := "compose action result was uncertain, existing resources confirmed the deployment"
		receipt.Outcome = controllerapply.OutcomeSucceeded
		receipt.Message = status
		logger.Warn("compose action result was uncertain but resources match the target revision",
			zap.Stringer("deploymentId", deployment.ID), zap.Error(err))
		return receipt, status, nil
	}

	if isAmbiguousEngineError(err) {
		receipt.Outcome = controllerapply.OutcomeUnknown
		receipt.Message = err.Error()
		return receipt, "", fmt.Errorf("%w: compose up: %v", errOutcomeUnknown, err)
	}

	receipt.Outcome = controllerapply.OutcomeFailed
	receipt.Message = err.Error()
	return receipt, "", fmt.Errorf("compose up failed: %w", err)
}

func applySwarmGeneration(
	ctx context.Context,
	deployment api.ControllerDeployment,
	updateStatus func(string),
) (*controllerapply.Receipt, string, error) {
	// Step 1 Ensure Docker Swarm is initialized
	initCmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Swarm.LocalNodeState}}")
	initOutput, err := initCmd.CombinedOutput()
	if err != nil {
		return nil, "", fmt.Errorf("failed to check Docker Swarm state: %w", err)
	}
	if !strings.Contains(strings.TrimSpace(string(initOutput)), "active") {
		return nil, "", fmt.Errorf("docker Swarm not initialized: %s", string(initOutput))
	}

	projectName, err := getProjectName(deployment.ComposeFile)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get project name from compose file: %w", err)
	}

	cleanedComposeFile, err := cleanComposeFile(deployment.ComposeFile)
	if err != nil {
		return nil, "", err
	}
	expectedServices := swarmExpectedServices(cleanedComposeFile)

	workDir, err := PublishSwarmProject(deployment, cleanedComposeFile, deployment.EnvFile)
	if err != nil {
		return nil, "", err
	}

	receipt := &controllerapply.Receipt{Action: controllerapply.ActionSwarmDeploy, StartedAt: receiptNow()}

	existingServices, inspectErr := inspectSwarmResources(ctx, projectName)
	if inspectErr == nil && swarmResourcesMatch(existingServices, projectName, expectedServices) {
		updateStatus("services already present")
		receipt.Outcome = controllerapply.OutcomeSucceeded
		receipt.Resources = swarmResourceIdentities(existingServices)
		receipt.FinishedAt = receiptNow()
		status := "swarm services already present, deploy not repeated"
		receipt.Message = status
		return receipt, status, nil
	}

	envVars := os.Environ()
	envVars = append(envVars, DockerConfigEnv(deployment)...)
	if deployment.EnvFile != nil {
		parsedEnv, err := dotenv.UnmarshalBytesWithLookup(deployment.EnvFile, nil)
		if err != nil {
			return nil, "", fmt.Errorf("failed to parse env file: %w", err)
		}
		for key, value := range parsedEnv {
			envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
		}
	}

	updateStatus("applying compose project")
	composeArgs := []string{
		"stack", "deploy",
		"--compose-file", workDir.ComposeFile,
		"--with-registry-auth",
		"--detach=true",
		projectName,
	}
	cmd := exec.CommandContext(ctx, "docker", composeArgs...)
	cmd.Dir = workDir.Path
	cmd.Env = envVars

	cmdOut, cmdErr := cmd.CombinedOutput()
	services, _ := inspectSwarmResources(ctx, projectName)
	receipt.Resources = swarmResourceIdentities(services)
	receipt.FinishedAt = receiptNow()

	if cmdErr == nil {
		status := string(cmdOut)
		receipt.Outcome = controllerapply.OutcomeSucceeded
		receipt.Message = status
		logger.Debug("docker stack deploy returned", zap.String("output", status))
		return receipt, status, nil
	}

	logger.Error("docker stack deploy failed", zap.String("output", string(cmdOut)))
	if isAmbiguousEngineError(cmdErr) && swarmResourcesMatch(services, projectName, expectedServices) {
		status := "swarm deploy result was uncertain, existing services confirmed the deployment"
		receipt.Outcome = controllerapply.OutcomeSucceeded
		receipt.Message = status
		logger.Warn("swarm deploy result was uncertain but services match the target revision",
			zap.Stringer("deploymentId", deployment.ID), zap.Error(cmdErr))
		return receipt, status, nil
	}

	if isAmbiguousEngineError(cmdErr) {
		receipt.Outcome = controllerapply.OutcomeUnknown
		receipt.Message = string(cmdOut)
		if receipt.Message == "" {
			receipt.Message = cmdErr.Error()
		}
		return receipt, "", fmt.Errorf("%w: docker stack deploy: %v", errOutcomeUnknown, cmdErr)
	}

	receipt.Outcome = controllerapply.OutcomeFailed
	receipt.Message = string(cmdOut)
	return receipt, "", errors.New(string(cmdOut))
}

// restartWithReceipt runs a forced restart and records the action and its resources in the generation so
// a restarted process does not force it a second time. A failed restart is still reported by the caller
// but never fails a successful apply.
func restartWithReceipt(ctx context.Context, deployment ControllerDeployment) (controllerapply.Receipt, error) {
	receipt := controllerapply.Receipt{StartedAt: receiptNow()}
	if deployment.DockerType == types.DockerTypeSwarm {
		receipt.Action = controllerapply.ActionSwarmRestart
	} else {
		receipt.Action = controllerapply.ActionComposeRestart
	}

	err := RunDockerRestart(ctx, deployment)
	receipt.FinishedAt = receiptNow()
	switch {
	case err == nil:
		receipt.Outcome = controllerapply.OutcomeSucceeded
	case isAmbiguousEngineError(err):
		receipt.Outcome = controllerapply.OutcomeUnknown
	default:
		receipt.Outcome = controllerapply.OutcomeFailed
	}
	receipt.Message = ""
	if err != nil {
		receipt.Message = err.Error()
	}

	switch deployment.DockerType {
	case types.DockerTypeSwarm:
		if services, inspectErr := inspectSwarmResources(ctx, deployment.ProjectName); inspectErr == nil {
			receipt.Resources = swarmResourceIdentities(services)
		}
	default:
		receipt.Resources = composeResourceIdentities(inspectComposeResources(ctx, deployment.ProjectName))
	}
	return receipt, err
}

func DockerEngineUninstall(ctx context.Context, deployment ControllerDeployment) error {
	if deployment.DockerType == types.DockerTypeSwarm {
		return UninstallDockerSwarm(ctx, deployment)
	}
	return UninstallDockerCompose(ctx, deployment)
}

// UninstallDockerCompose tears a deployment down. Compose runs the pre_stop hook of a service only where
// it is given the project, which it cannot reconstruct from the container labels, so the first Down is the
// one that runs the hooks. It is best effort: a hook that fails aborts that Down with its containers still
// running. The second Down is the authoritative teardown and additionally removes volumes and networks
// that carry the project label but are no longer declared in the project of the last applied revision.
func UninstallDockerCompose(ctx context.Context, deployment ControllerDeployment) error {
	logger := logger.With(zap.Stringer("deploymentId", deployment.ID))

	if project, err := LoadComposeProject(ctx, deployment); err != nil {
		logger.Warn("could not load compose project, skipping lifecycle hooks", zap.Error(err))
	} else if err := composeService.Down(ctx, deployment.ProjectName, composeapi.DownOptions{
		Project:       project,
		Volumes:       true,
		RemoveOrphans: true,
	}); err != nil {
		logger.Warn("graceful teardown failed, removing the deployment anyway", zap.Error(err))
	}

	return composeService.Down(ctx, deployment.ProjectName, composeapi.DownOptions{
		Volumes:       true,
		RemoveOrphans: true,
	})
}

func UninstallDockerSwarm(ctx context.Context, deployment ControllerDeployment) error {
	cmd := exec.CommandContext(ctx, "docker", "stack", "rm", deployment.ProjectName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove Docker Swarm stack: %w: %v", err, string(out))
	}

	// Optional: Prune unused networks created by Swarm
	pruneCmd := exec.CommandContext(ctx, "docker", "network", "prune", "-f")
	pruneOut, pruneErr := pruneCmd.CombinedOutput()
	if pruneErr != nil {
		logger.Warn("Failed to prune networks", zap.String("output", string(pruneOut)), zap.Error(pruneErr))
	}

	return nil
}

func cleanComposeFile(composeData []byte) ([]byte, error) {
	if compose, err := DecodeComposeFile(composeData); err != nil {
		return nil, err
	} else {
		delete(compose, "name")
		return EncodeComposeFile(compose)
	}
}

func DockerConfigEnv(deployment api.ControllerDeployment) []string {
	if len(deployment.RegistryAuth) > 0 || hasRegistryImages(deployment) {
		return []string{
			dockerconfig.EnvOverrideConfigDir + "=" + controllerauth.DockerConfigDir(deployment),
		}
	} else {
		return nil
	}
}

// hasRegistryImages parses the compose file in order to check whether one of the services uses an image hosted on
// [controllerenv.DistrRegistryHost].
func hasRegistryImages(deployment api.ControllerDeployment) bool {
	for _, image := range specServiceImages(deployment.ComposeFile) {
		if strings.HasPrefix(image, controllerenv.DistrRegistryHost) {
			return true
		}
	}
	return false
}
