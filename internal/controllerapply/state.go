package controllerapply

import (
	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
)

type State string

const (
	StateUnspecified State = ""
	StateProgressing State = "progressing"
	StateReady       State = "ready"
	StateFailed      State = "failed"
)

// DeploymentState is the committed view of one deployment that the controller publishes atomically.
// A state file is only ever replaced as a whole, so readers never observe a torn write. GenerationID
// ties the published state to the application generation that produced it; a superseded generation
// cannot overwrite newer state.
type DeploymentState struct {
	ID                uuid.UUID        `json:"id"`
	RevisionID        uuid.UUID        `json:"revisionId"`
	CurrentRevisionID uuid.UUID        `json:"currentRevisionId,omitzero"`
	GenerationID      uuid.UUID        `json:"generationId,omitzero"`
	ProjectName       string           `json:"projectName"`
	DockerType        types.DockerType `json:"docker_type,omitempty"`
	State             State            `json:"phase"`
}

func (d DeploymentState) GetDeploymentID() uuid.UUID {
	return d.ID
}

func (d DeploymentState) GetDeploymentRevisionID() uuid.UUID {
	return d.RevisionID
}

// AppliedRevisionID returns the revision that was last applied successfully or uuid.Nil if there is none.
// State saved by controllers that did not know CurrentRevisionID or a generation yet only has a
// RevisionID, which is the applied one unless applying it failed or is still in progress.
func (d DeploymentState) AppliedRevisionID() uuid.UUID {
	if d.CurrentRevisionID != uuid.Nil {
		return d.CurrentRevisionID
	}
	if d.State == StateReady || d.State == StateUnspecified {
		return d.RevisionID
	}
	return uuid.Nil
}
