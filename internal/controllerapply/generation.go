// Package controllerapply owns the on-disk convergence primitives of a deployment target controller:
// application generations, atomic state and project-file publication, external result receipts and
// advisory deployment locks. The Docker controller wires engine operations onto these primitives.
package controllerapply

import (
	"time"

	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
)

type Action string

const (
	ActionComposeUp      Action = "compose-up"
	ActionSwarmDeploy    Action = "swarm-deploy"
	ActionComposeRestart Action = "compose-restart"
	ActionSwarmRestart   Action = "swarm-restart"
	ActionEngineRun      Action = "engine-run"
)

type Outcome string

const (
	OutcomeUnspecified Outcome = ""
	OutcomeSucceeded   Outcome = "succeeded"
	OutcomeFailed      Outcome = "failed"
	// OutcomeUnknown records an engine response that may or may not have changed resources, for example a
	// timeout or a lost connection after the request was sent. The resources have to be inspected before
	// the generation is retried or failed.
	OutcomeUnknown Outcome = "unknown"
)

type GenerationPhase string

const (
	// PhaseClaimed covers everything from claiming the generation until the engine result is settled,
	// including a process that died while applying.
	PhaseClaimed GenerationPhase = "claimed"
	// PhaseFinalized means state was published and the only thing possibly outstanding is the server
	// callback, which Receipt.Reported tracks.
	PhaseFinalized GenerationPhase = "finalized"
)

// ResourceIdentity identifies one resource observed on the container engine after an action. Receipts
// let a restarted or unsure controller compare what it intended against what actually exists instead of
// applying again.
type ResourceIdentity struct {
	Kind     string            `json:"kind"`
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name,omitempty"`
	Image    string            `json:"image,omitempty"`
	State    string            `json:"state,omitempty"`
	Networks []string          `json:"networks,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// Receipt is the durable record of one engine action: what was attempted, what the engine reported and
// which resources existed afterwards. The primary receipt of a generation also carries the pending final
// status callback.
type Receipt struct {
	Action   Action             `json:"action"`
	Outcome  Outcome            `json:"outcome"`
	Message  string             `json:"message,omitempty"`
	Resources []ResourceIdentity `json:"resources,omitempty"`
	StartedAt time.Time          `json:"startedAt"`
	FinishedAt time.Time        `json:"finishedAt,omitempty"`

	// Reported says the final deployment status derived from this receipt was delivered to the server.
	// While false a successful apply still stays successful: the callback is retried from this receipt
	// instead of being reinterpreted as a failure.
	Reported      bool                       `json:"reported"`
	ReportType    types.DeploymentStatusType `json:"reportType,omitempty"`
	ReportMessage string                     `json:"reportMessage,omitempty"`
}

// Generation is one unique attempt to apply one target revision. Exactly one live generation exists per
// deployment at a time; restarts and concurrent processes adopt it instead of claiming a second one.
type Generation struct {
	ID           uuid.UUID         `json:"id"`
	DeploymentID uuid.UUID         `json:"deploymentId"`
	RevisionID   uuid.UUID         `json:"revisionId"`
	ProjectName  string            `json:"projectName"`
	DockerType   types.DockerType  `json:"dockerType,omitempty"`
	Phase        GenerationPhase   `json:"phase"`
	ClaimedAt    time.Time         `json:"claimedAt"`
	Receipt      *Receipt          `json:"receipt,omitempty"`
	// Actions records further engine actions of the same generation, for example a forced restart.
	Actions []Receipt `json:"actions,omitempty"`
}

// Target identifies the revision a generation is claimed for.
type Target struct {
	DeploymentID uuid.UUID
	RevisionID   uuid.UUID
	ProjectName  string
	DockerType   types.DockerType
}

func (g *Generation) isLive() bool {
	if g.Phase != PhaseFinalized {
		return true
	}
	return g.Receipt != nil && !g.Receipt.Reported
}
