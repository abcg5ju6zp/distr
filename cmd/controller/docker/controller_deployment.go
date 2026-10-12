package main

import (
	"errors"

	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/controllerapply"
	"github.com/google/uuid"
)

type State = controllerapply.State

const (
	StateUnspecified = controllerapply.StateUnspecified
	StateProgressing = controllerapply.StateProgressing
	StateReady       = controllerapply.StateReady
	StateFailed      = controllerapply.StateFailed
)

// ControllerDeployment is the persisted deployment state owned by the controllerapply package. It is
// aliased so existing call sites keep working while state is published atomically under a generation.
type ControllerDeployment = controllerapply.DeploymentState

func NewControllerDeployment(
	deployment api.ControllerDeployment,
	previous *ControllerDeployment,
	generationID uuid.UUID,
) (*ControllerDeployment, error) {
	name, err := getProjectName(deployment.ComposeFile)
	if err != nil {
		return nil, err
	}
	result := &ControllerDeployment{
		ID:           deployment.ID,
		RevisionID:   deployment.RevisionID,
		ProjectName:  name,
		DockerType:   *deployment.DockerType,
		GenerationID: generationID,
	}
	if previous != nil && previous.ID == deployment.ID {
		result.CurrentRevisionID = previous.AppliedRevisionID()
	}
	return result, nil
}

func getProjectName(data []byte) (string, error) {
	compose, err := DecodeComposeFile(data)
	if err != nil {
		return "", err
	}
	name, ok := compose["name"].(string)
	if !ok {
		return "", errors.New("name is not a string")
	}
	return name, nil
}

// GetExistingDeployments loads the committed state of every deployment. Corrupt state files are
// quarantined and returned separately instead of stopping the remaining deployments from loading.
func GetExistingDeployments() (
	map[uuid.UUID]ControllerDeployment,
	[]controllerapply.Corruption,
	error,
) {
	return deploymentStore.LoadStates()
}

func DeleteDeployment(deployment ControllerDeployment) error {
	return deploymentStore.DeleteState(deployment.ID)
}
