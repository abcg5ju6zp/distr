package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/controllerapply"
	"github.com/distr-sh/distr/internal/types"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/google/uuid"
)

const (
	composeFileName = "docker-compose.yaml"
	envFileName     = ".env"
)

// ComposeWorkingDir describes a directory that holds a deployment's compose file (and optional env file)
// under their canonical names, so that relative paths referenced from the compose file resolve.
type ComposeWorkingDir struct {
	// Path is the working directory containing the files.
	Path string
	// ComposeFile is the absolute path to the written compose file.
	ComposeFile string
	// EnvFile is the absolute path to the written env file, or empty if the deployment has no env file.
	EnvFile string
}

// ComposeProjectDir is the directory a deployment's compose file and env file are kept in for as long as
// the deployment exists. Running the lifecycle hooks of a project on teardown requires its compose file,
// and the deployment is gone from the resource response by then, so the files have to outlive the apply.
func ComposeProjectDir(id uuid.UUID) string {
	return deploymentStore.ProjectDir(id)
}

// PublishComposeProject stages the compose and env files in a temporary directory, validates the full
// project from staging and only then atomically publishes them, so a failed or interrupted write never
// replaces the last readable revision.
func PublishComposeProject(ctx context.Context, deployment api.ControllerDeployment) (ComposeWorkingDir, error) {
	staging, err := deploymentStore.StagingDir(deployment.ID)
	if err != nil {
		return ComposeWorkingDir{}, fmt.Errorf("failed to create compose staging directory: %w", err)
	}
	defer func() { _ = controllerapply.RemoveStagingDir(staging) }()

	staged, err := stageComposeFiles(staging, deployment)
	if err != nil {
		return ComposeWorkingDir{}, err
	}

	svc, err := ComposeServiceForDeployment(deployment)
	if err != nil {
		return ComposeWorkingDir{}, err
	}
	if _, err := svc.LoadProject(ctx, ComposeLoadOptions(staged)); err != nil {
		return ComposeWorkingDir{}, fmt.Errorf("failed to validate compose project: %w", err)
	}

	return commitComposeFiles(deployment, staging)
}

// PublishSwarmProject publishes the name-stripped compose file "docker stack deploy" consumes.
func PublishSwarmProject(deployment api.ControllerDeployment, composeFile, envFile []byte) (ComposeWorkingDir, error) {
	staging, err := deploymentStore.StagingDir(deployment.ID)
	if err != nil {
		return ComposeWorkingDir{}, fmt.Errorf("failed to create compose staging directory: %w", err)
	}
	defer func() { _ = controllerapply.RemoveStagingDir(staging) }()

	stagedDeployment := api.ControllerDeployment{ID: deployment.ID, ComposeFile: composeFile, EnvFile: envFile}
	if _, err := stageComposeFiles(staging, stagedDeployment); err != nil {
		return ComposeWorkingDir{}, err
	}
	if _, err := DecodeComposeFile(composeFile); err != nil {
		return ComposeWorkingDir{}, fmt.Errorf("failed to validate swarm compose file: %w", err)
	}
	return commitComposeFiles(stagedDeployment, staging)
}

// stageComposeFiles writes both files into the staging directory and fsyncs them without publishing.
func stageComposeFiles(staging string, deployment api.ControllerDeployment) (ComposeWorkingDir, error) {
	if err := controllerapply.WriteStagedFile(staging, composeFileName, deployment.ComposeFile); err != nil {
		return ComposeWorkingDir{}, fmt.Errorf("failed to write compose file: %w", err)
	}
	result := ComposeWorkingDir{
		Path:        staging,
		ComposeFile: filepath.Join(staging, composeFileName),
	}
	if deployment.EnvFile != nil {
		if err := controllerapply.WriteStagedFile(staging, envFileName, deployment.EnvFile); err != nil {
			return ComposeWorkingDir{}, fmt.Errorf("failed to write env file: %w", err)
		}
		result.EnvFile = filepath.Join(staging, envFileName)
	}
	return result, nil
}

// commitComposeFiles publishes the env file first and the compose file last, so an interrupted publish
// can never pair the new compose file with the previous revision's env file.
func commitComposeFiles(deployment api.ControllerDeployment, staging string) (ComposeWorkingDir, error) {
	if deployment.EnvFile == nil {
		if err := deploymentStore.RemoveProjectFile(deployment.ID, staging, envFileName); err != nil {
			return ComposeWorkingDir{}, fmt.Errorf("failed to remove env file: %w", err)
		}
	} else if err := deploymentStore.CommitProjectFile(deployment.ID, staging, envFileName); err != nil {
		return ComposeWorkingDir{}, err
	}
	if err := deploymentStore.CommitProjectFile(deployment.ID, staging, composeFileName); err != nil {
		return ComposeWorkingDir{}, err
	}
	return ReadComposeProjectDir(deployment.ID)
}

// EnsureComposeProjectDir writes the project files unless they already exist, which covers a deployment
// applied by a controller version that did not keep them yet and would otherwise never write them again.
func EnsureComposeProjectDir(ctx context.Context, deployment api.ControllerDeployment) error {
	_, err := os.Stat(filepath.Join(ComposeProjectDir(deployment.ID), composeFileName))
	if err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err = PublishComposeProject(ctx, deployment)
	return err
}

func ReadComposeProjectDir(id uuid.UUID) (ComposeWorkingDir, error) {
	dir := ComposeProjectDir(id)
	result := ComposeWorkingDir{Path: dir, ComposeFile: filepath.Join(dir, composeFileName)}
	if _, err := os.Stat(result.ComposeFile); err != nil {
		return ComposeWorkingDir{}, err
	}

	envFile := filepath.Join(dir, envFileName)
	if _, err := os.Stat(envFile); err == nil {
		result.EnvFile = envFile
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ComposeWorkingDir{}, err
	}

	return result, nil
}

func DeleteComposeProjectDir(id uuid.UUID) error {
	return os.RemoveAll(ComposeProjectDir(id))
}

func ComposeLoadOptions(workDir ComposeWorkingDir) composeapi.ProjectLoadOptions {
	opts := composeapi.ProjectLoadOptions{
		WorkingDir:  workDir.Path,
		ConfigPaths: []string{workDir.ComposeFile},
	}
	if workDir.EnvFile != "" {
		opts.EnvFiles = []string{workDir.EnvFile}
	}
	return opts
}

// LoadComposeProject loads the project of an applied deployment from its project directory. Compose runs
// the lifecycle hooks of a service only where it is given the service definition, which it cannot
// reconstruct from the container labels, so every operation that should run them has to pass the project.
// A Swarm deployment is rejected: it persists the name-stripped file that "docker stack deploy" consumes,
// so the project would end up named after the directory rather than after the deployment.
func LoadComposeProject(ctx context.Context, deployment ControllerDeployment) (*composetypes.Project, error) {
	if deployment.DockerType != types.DockerTypeCompose {
		return nil, fmt.Errorf("cannot load the compose project of a %v deployment", deployment.DockerType)
	}

	workDir, err := ReadComposeProjectDir(deployment.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to read compose project directory: %w", err)
	}
	project, err := composeService.LoadProject(ctx, ComposeLoadOptions(workDir))
	if err != nil {
		return nil, fmt.Errorf("failed to load compose project: %w", err)
	}
	return project, nil
}
