package mapping

import (
	"time"

	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
)

func overviewUserToAPI(name *string, imageID *uuid.UUID) *api.OverviewUser {
	if name == nil {
		return nil
	}
	return &api.OverviewUser{Name: *name, ImageURL: CreateImageURL(imageID)}
}

func OverviewDeploymentTargetToAPI(
	target types.DeploymentTarget,
	deployments []api.OverviewDeployment,
) api.OverviewDeploymentTarget {
	if deployments == nil {
		deployments = []api.OverviewDeployment{}
	}
	return api.OverviewDeploymentTarget{
		ID:          target.ID,
		Name:        target.Name,
		Type:        target.Type,
		Connected:   target.ReportedControllerVersionID != nil,
		Deployments: deployments,
	}
}

func OverviewDeploymentToAPI(
	d types.OverviewDeployment,
	latest *types.ApplicationVersion,
	updateAvailable bool,
) api.OverviewDeployment {
	result := api.OverviewDeployment{
		ID:                            d.ID,
		ApplicationID:                 d.Application.ID,
		ApplicationName:               d.Application.Name,
		ApplicationImageURL:           CreateImageURL(d.Application.ImageID),
		ApplicationTitleImageURL:      CreateImageURL(d.Application.TitleImageID),
		ApplicationLink:               d.ApplicationLink,
		ApplicationVersionID:          d.ApplicationVersionID,
		ApplicationVersionName:        d.ApplicationVersionName,
		CurrentApplicationVersionID:   d.CurrentApplicationVersionID,
		CurrentApplicationVersionName: d.CurrentApplicationVersionName,
		UpdateAvailable:               updateAvailable,
		DeploymentRevisionID:          d.DeploymentRevisionID,
		DeploymentRevisionCreatedAt:   d.DeploymentRevisionCreatedAt,
		DeploymentRevisionCreatedBy:   overviewUserToAPI(d.CreatedByName, d.CreatedByImageID),
		CurrentDeploymentRevisionID:   d.CurrentDeploymentRevisionID,
	}
	if latest != nil {
		result.LatestApplicationVersionName = &latest.Name
	}
	if d.LatestStatus != nil {
		result.LatestStatus = new(DeploymentRevisionStatusToAPI(*d.LatestStatus))
	}
	if d.CurrentStatus != nil {
		result.CurrentStatus = new(DeploymentRevisionStatusToAPI(*d.CurrentStatus))
	}
	return result
}

func OverviewArtifactToAPI(
	a types.ArtifactWithDownloads,
	latest *types.ArtifactVersion,
	pull *types.OverviewArtifactPull,
) api.OverviewArtifact {
	result := api.OverviewArtifact{
		ID:            a.ID,
		Name:          a.Name,
		ImageURL:      CreateImageURL(a.ImageID),
		TitleImageURL: CreateImageURL(a.TitleImageID),
	}
	if latest != nil {
		result.LatestVersionName = &latest.Name
	}
	if pull != nil {
		result.LastPull = &api.OverviewArtifactPull{
			VersionName:          pull.VersionName,
			CreatedAt:            pull.CreatedAt,
			PulledBy:             overviewUserToAPI(pull.UserName, pull.UserImageID),
			DeploymentTargetName: pull.DeploymentTargetName,
			IsLatest:             latest == nil || latest.ManifestBlobDigest == pull.ManifestBlobDigest,
		}
	}
	return result
}

func OverviewChecklistToAPI(c types.OverviewChecklist) api.OverviewChecklistResponse {
	return api.OverviewChecklistResponse{
		ProfileCompleted:   c.ProfileCompleted,
		TeamMembersInvited: c.TeamMembersInvited,
		DeploymentCreated:  c.DeploymentCreated,
		SecretCreated:      c.SecretCreated,
		AccessTokenCreated: c.AccessTokenCreated,
		ArtifactPulled:     c.ArtifactPulled,
	}
}

func OverviewLicenseToAPI(l types.OverviewLicense) api.OverviewLicenseResponse {
	return api.OverviewLicenseResponse{Name: l.Name, NotBefore: l.NotBefore, ExpiresAt: l.ExpiresAt}
}

func OverviewAlertSummaryToAPI(s types.OverviewAlertSummary) api.OverviewAlertsResponse {
	return api.OverviewAlertsResponse{
		AlertConfigurationCount: s.AlertConfigurationCount,
		NotificationCount:       s.NotificationCount,
	}
}

func OverviewAccessTokenToAPI(t types.AccessToken, expiresAt time.Time) api.OverviewAccessToken {
	return api.OverviewAccessToken{ID: t.ID, Label: t.Label, ExpiresAt: expiresAt}
}

func OverviewSupportBundleToAPI(b types.SupportBundleWithDetails) api.OverviewSupportBundle {
	result := api.OverviewSupportBundle{
		ID:        b.ID,
		Title:     b.Title,
		Status:    b.Status,
		UpdatedAt: b.CreatedAt,
		UpdatedBy: overviewUserToAPI(b.CreatedByUserName, b.CreatedByImageID),
	}
	if b.StatusChangedAt != nil {
		result.UpdatedAt = *b.StatusChangedAt
		result.UpdatedBy = overviewUserToAPI(b.StatusChangedByUserName, b.StatusChangedByImageID)
	}
	return result
}

func OverviewActivityToAPI(a types.OverviewActivity) api.OverviewActivity {
	return api.OverviewActivity{
		ID:        a.ID,
		CreatedAt: a.CreatedAt,
		Type:      a.Type,
		Subject:   a.Subject,
		Detail:    a.Detail,
		User:      overviewUserToAPI(a.UserName, a.UserImageID),
	}
}
