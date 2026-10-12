package api

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/distr-sh/distr/internal/types"
	"github.com/distr-sh/distr/internal/validation"
	"github.com/google/uuid"
)

var envVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Configuration

type SupportBundleConfigurationEnvVar struct {
	Name     string `json:"name"`
	Redacted bool   `json:"redacted"`
}

type CreateUpdateSupportBundleConfigurationRequest struct {
	EnvVars []SupportBundleConfigurationEnvVar `json:"envVars"`
}

func (r *CreateUpdateSupportBundleConfigurationRequest) Validate() error {
	seen := make(map[string]struct{}, len(r.EnvVars))
	for _, ev := range r.EnvVars {
		if ev.Name == "" {
			return validation.NewValidationFailedError("environment variable name must not be empty")
		}
		if !envVarNamePattern.MatchString(ev.Name) {
			return validation.NewValidationFailedError(
				fmt.Sprintf("invalid environment variable name: %v (must match [A-Za-z_][A-Za-z0-9_]*)", ev.Name))
		}
		key := strings.ToLower(ev.Name)
		if _, exists := seen[key]; exists {
			return validation.NewValidationFailedError(
				fmt.Sprintf("duplicate environment variable name: %v", ev.Name))
		}
		seen[key] = struct{}{}
	}
	return nil
}

var scriptNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]*$`)

// resourceIdentityPattern matches both the resource name and the stable key. Container names and
// the custom-script:<uuid> keys stay within this charset, so no client-controlled value can carry
// a newline into the resume state or a path separator into a ZIP entry.
var resourceIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:-]*$`)

var resourceDigestPattern = regexp.MustCompile(`^[A-Fa-f0-9]{64}$`)

const (
	maxResourceNameLength      = 128
	maxResourceStableKeyLength = 160
	maxFailureReasonLength     = 512
)

const (
	maxScriptNameLength        = 64
	maxScriptDescriptionLength = 256
	maxScriptContentBytes      = 64 * 1024
)

type SupportBundleConfigurationScript struct {
	ID          uuid.UUID `json:"id"`
	CreatedAt   time.Time `json:"createdAt"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	Content     string    `json:"content"`
	Enabled     bool      `json:"enabled"`
}

type CreateUpdateSupportBundleConfigurationScriptRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Content     string  `json:"content"`
	Enabled     bool    `json:"enabled"`
}

func (r *CreateUpdateSupportBundleConfigurationScriptRequest) Validate() error {
	if r.Name == "" {
		return validation.NewValidationFailedError("script name must not be empty")
	}
	if len(r.Name) > maxScriptNameLength {
		return validation.NewValidationFailedError(
			fmt.Sprintf("script name must not be longer than %v characters", maxScriptNameLength))
	}
	if !scriptNamePattern.MatchString(r.Name) {
		return validation.NewValidationFailedError(
			"script name must start with a letter or digit and may only contain letters, digits, " +
				"spaces, dots, dashes and underscores")
	}
	if r.Description != nil {
		if *r.Description == "" {
			r.Description = nil
		} else if len(*r.Description) > maxScriptDescriptionLength {
			return validation.NewValidationFailedError(
				fmt.Sprintf("script description must not be longer than %v characters", maxScriptDescriptionLength))
		}
	}
	if r.Content == "" {
		return validation.NewValidationFailedError("script content must not be empty")
	}
	if len(r.Content) > maxScriptContentBytes {
		return validation.NewValidationFailedError(
			fmt.Sprintf("script content must not be larger than %v KiB", maxScriptContentBytes/1024))
	}
	return nil
}

// Bundle

type SupportBundle struct {
	ID                       uuid.UUID  `json:"id"`
	CreatedAt                time.Time  `json:"createdAt"`
	CustomerOrganizationID   uuid.UUID  `json:"customerOrganizationId"`
	CustomerOrganizationName string     `json:"customerOrganizationName"`
	CreatedByUserAccountID   *uuid.UUID `json:"createdByUserAccountId,omitempty"`
	CreatedByUserName        *string    `json:"createdByUserName,omitempty"`
	CreatedByImageURL        *string    `json:"createdByImageUrl,omitempty"`
	Title                    string     `json:"title"`
	Description              *string    `json:"description,omitempty"`
	Status                   string     `json:"status"`
	ResourceCount            int64      `json:"resourceCount"`
	CommentCount             int64      `json:"commentCount"`
	LastCommentAt            *time.Time `json:"lastCommentAt,omitempty"`
	StatusChangedByUserName  *string    `json:"statusChangedByUserName,omitempty"`
	StatusChangedByImageURL  *string    `json:"statusChangedByImageUrl,omitempty"`
	StatusChangedAt          *time.Time `json:"statusChangedAt,omitempty"`
}

type SupportBundleDetail struct {
	SupportBundle
	Resources      []SupportBundleResource `json:"resources"`
	Comments       []SupportBundleComment  `json:"comments"`
	CollectCommand *string                 `json:"collectCommand,omitempty"`
}

type CreateSupportBundleRequest struct {
	Title       string  `json:"title"`
	Description *string `json:"description"`
}

type CreateSupportBundleResponse struct {
	SupportBundle
	CollectCommand string `json:"collectCommand"`
}

type UpdateSupportBundleStatusRequest struct {
	Status string `json:"status"`
}

// Collection

type SupportBundleResourceStatus string

const (
	SupportBundleResourceStatusPending   SupportBundleResourceStatus = "pending"
	SupportBundleResourceStatusSucceeded SupportBundleResourceStatus = "succeeded"
	SupportBundleResourceStatusSkipped   SupportBundleResourceStatus = "skipped"
	SupportBundleResourceStatusOversized SupportBundleResourceStatus = "oversized"
	SupportBundleResourceStatusFailed    SupportBundleResourceStatus = "failed"
)

type SupportBundleResourceKind string

const (
	SupportBundleResourceKindSystemInfo    SupportBundleResourceKind = "system_info"
	SupportBundleResourceKindHostEnv       SupportBundleResourceKind = "host_env"
	SupportBundleResourceKindCustomScript  SupportBundleResourceKind = "custom_script"
	SupportBundleResourceKindContainerEnv  SupportBundleResourceKind = "container_env"
	SupportBundleResourceKindContainerLogs SupportBundleResourceKind = "container_logs"
	SupportBundleResourceKindOther         SupportBundleResourceKind = "other"
)

// Resource

type SupportBundleResource struct {
	ID            uuid.UUID                   `json:"id"`
	CreatedAt     time.Time                   `json:"createdAt"`
	StableKey     string                      `json:"stableKey"`
	Name          string                      `json:"name"`
	Kind          SupportBundleResourceKind   `json:"kind"`
	Required      bool                        `json:"required"`
	Status        SupportBundleResourceStatus `json:"status"`
	SizeBytes     int64                       `json:"sizeBytes"`
	ContentSHA256 *string                     `json:"contentSha256,omitempty"`
	FailureReason *string                     `json:"failureReason,omitempty"`
	Content       string                      `json:"content"`
}

// ReportSupportBundleResourceRequest is the multipart form posted by the collect script for one
// resource outcome. StableKey makes retries idempotent and ContentSHA256 makes them
// content-addressed.
type ReportSupportBundleResourceRequest struct {
	StableKey     string
	Name          string
	Kind          SupportBundleResourceKind
	Status        SupportBundleResourceStatus
	ContentSHA256 string
	FailureReason string
}

// Validate checks the multipart-independent fields of a resource outcome report. The presence of
// the content file for the reported status is validated by the handler.
func (r *ReportSupportBundleResourceRequest) Validate() error {
	if r.StableKey == "" {
		return validation.NewValidationFailedError("stableKey is required")
	}
	if len(r.StableKey) > maxResourceStableKeyLength || !resourceIdentityPattern.MatchString(r.StableKey) {
		return validation.NewValidationFailedError(
			"stableKey must start with a letter or digit and may only contain letters, digits, " +
				"spaces, dots, dashes, underscores and colons")
	}
	if r.Name == "" {
		return validation.NewValidationFailedError("name is required")
	}
	if len(r.Name) > maxResourceNameLength || !resourceIdentityPattern.MatchString(r.Name) {
		return validation.NewValidationFailedError(
			"name must start with a letter or digit and may only contain letters, digits, " +
				"spaces, dots, dashes and underscores")
	}
	if r.Status == "" {
		r.Status = SupportBundleResourceStatusSucceeded
	}
	switch r.Status {
	case SupportBundleResourceStatusSucceeded,
		SupportBundleResourceStatusSkipped,
		SupportBundleResourceStatusOversized,
		SupportBundleResourceStatusFailed:
	default:
		return validation.NewValidationFailedError(fmt.Sprintf("invalid resource status: %v", r.Status))
	}
	if r.Kind != "" {
		switch r.Kind {
		case SupportBundleResourceKindSystemInfo,
			SupportBundleResourceKindHostEnv,
			SupportBundleResourceKindCustomScript,
			SupportBundleResourceKindContainerEnv,
			SupportBundleResourceKindContainerLogs,
			SupportBundleResourceKindOther:
		default:
			return validation.NewValidationFailedError(fmt.Sprintf("invalid resource kind: %v", r.Kind))
		}
	}
	if r.ContentSHA256 != "" && !resourceDigestPattern.MatchString(r.ContentSHA256) {
		return validation.NewValidationFailedError("sha256 must be 64 hexadecimal characters")
	}
	if len(r.FailureReason) > maxFailureReasonLength {
		return validation.NewValidationFailedError(
			fmt.Sprintf("failureReason must not be longer than %v characters", maxFailureReasonLength))
	}
	return nil
}

const (
	// SupportBundleResourceOutcomeStored is returned when the reported outcome was recorded.
	SupportBundleResourceOutcomeStored = types.SupportBundleResourceOutcomeStored
	// SupportBundleResourceOutcomeDuplicate is returned for an idempotent retry of an already
	// recorded resource.
	SupportBundleResourceOutcomeDuplicate = types.SupportBundleResourceOutcomeDuplicate
	// SupportBundleResourceOutcomeOversized is returned when the content exceeded the per-resource
	// limit and was not stored.
	SupportBundleResourceOutcomeOversized = types.SupportBundleResourceOutcomeOversized
)

type ReportSupportBundleResourceResponse struct {
	ID            uuid.UUID                   `json:"id"`
	CreatedAt     time.Time                   `json:"createdAt"`
	StableKey     string                      `json:"stableKey"`
	Name          string                      `json:"name"`
	Kind          SupportBundleResourceKind   `json:"kind"`
	Required      bool                        `json:"required"`
	Status        SupportBundleResourceStatus `json:"status"`
	Outcome       string                      `json:"outcome"`
	SizeBytes     int64                       `json:"sizeBytes"`
	ContentSHA256 *string                     `json:"contentSha256,omitempty"`
	FailureReason *string                     `json:"failureReason,omitempty"`
}

type SupportBundleResourceCounts struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Skipped   int `json:"skipped"`
	Oversized int `json:"oversized"`
	Failed    int `json:"failed"`
}

// FinalizeSupportBundleResponse is the stable result of finalize: a retried finalize after a lost
// response gets the same body, including Replayed.
type FinalizeSupportBundleResponse struct {
	Status               string                        `json:"status"`
	CollectionGeneration uuid.UUID                     `json:"collectionGeneration"`
	PublishedAt          time.Time                     `json:"publishedAt"`
	Replayed             bool                          `json:"replayed"`
	Resources            SupportBundleResourceCounts   `json:"resources"`
}

// FinalizeSupportBundleConflictResponse explains why finalize did not publish: required resources
// are still in a non-terminal state.
type FinalizeSupportBundleConflictResponse struct {
	Error            string   `json:"error"`
	MissingResources []string `json:"missingResources"`
}

// SupportBundleResumeResource is one line of the text/plain resume state: stable key, status and
// digest joined by tabs, so a POSIX shell can parse it without a JSON parser.
type SupportBundleResumeResource struct {
	StableKey     string
	Status        SupportBundleResourceStatus
	ContentSHA256 *string
}

// Comment

type SupportBundleComment struct {
	ID            uuid.UUID  `json:"id"`
	CreatedAt     time.Time  `json:"createdAt"`
	UserAccountID *uuid.UUID `json:"userAccountId,omitempty"`
	UserName      *string    `json:"userName,omitempty"`
	UserImageURL  *string    `json:"userImageUrl,omitempty"`
	Content       string     `json:"content"`
}

type CreateSupportBundleCommentRequest struct {
	Content string `json:"content"`
}
