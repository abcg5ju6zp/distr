package handlers

import (
	"net/http"
	"slices"
	"time"

	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/auth"
	"github.com/distr-sh/distr/internal/db"
	"github.com/distr-sh/distr/internal/mapping"
	"github.com/distr-sh/distr/internal/middleware"
	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
	"github.com/oaswrap/spec/adapter/chiopenapi"
	"github.com/oaswrap/spec/option"
)

const overviewAccessTokenExpiryWindow = 7 * 24 * time.Hour

func OverviewRouter(r chiopenapi.Router) {
	r.WithOptions(option.GroupHidden(true))
	r.Use(middleware.RequireOrgAndRole, middleware.RequireCustomer)
	r.With(middleware.CustomerFeatureMiddleware(types.CustomerOrganizationFeatureDeploymentTargets)).
		Get("/deployment-targets", getOverviewDeploymentTargets)
	r.With(middleware.CustomerFeatureMiddleware(types.CustomerOrganizationFeatureArtifacts)).
		Get("/artifacts", getOverviewArtifacts)
	r.Get("/checklist", getOverviewChecklist)
	r.With(middleware.UseReadonlyDB).Get("/license", getOverviewLicense)
	r.With(middleware.ProFeature, middleware.AlertsCustomerFeatureMiddleware, middleware.UseReadonlyDB).
		Get("/alerts", getOverviewAlerts)
	r.Get("/access-tokens", getOverviewAccessTokens)
	r.With(
		middleware.ProFeature,
		middleware.CustomerFeatureMiddleware(types.CustomerOrganizationFeatureSupportBundles),
	).Get("/support-bundles", getOverviewSupportBundles)
	r.With(middleware.UseReadonlyDB).Get("/activity", getOverviewActivity)
}

func getOverviewDeploymentTargets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	orgID, customerOrgID := *auth.CurrentOrgID(), *auth.CurrentCustomerOrgID()

	targets, err := db.GetDeploymentTargetsByScope(ctx, orgID, &customerOrgID)
	if err != nil {
		respondInternalError(w, r, err, "failed to get deployment targets")
		return
	}
	deployments, err := db.GetOverviewDeployments(ctx, orgID, customerOrgID)
	if err != nil {
		respondInternalError(w, r, err, "failed to get deployments")
		return
	}
	applications, err := db.GetApplicationsByOrgID(ctx, orgID)
	if err != nil {
		respondInternalError(w, r, err, "failed to get applications")
		return
	}
	entitlements, err := db.GetApplicationEntitlementsWithCustomerOrganizationID(ctx, customerOrgID, orgID, nil)
	if err != nil {
		respondInternalError(w, r, err, "failed to get application entitlements")
		return
	}

	deploymentsByTarget := make(map[uuid.UUID][]api.OverviewDeployment)
	for _, d := range deployments {
		var latest *types.ApplicationVersion
		updateAvailable := false
		if i := slices.IndexFunc(applications, func(a types.Application) bool { return a.ID == d.Application.ID }); i >= 0 {
			var entitlement *types.ApplicationEntitlementWithVersions
			if d.ApplicationEntitlementID != nil {
				if j := slices.IndexFunc(entitlements, func(e types.ApplicationEntitlement) bool {
					return e.ID == *d.ApplicationEntitlementID
				}); j >= 0 {
					entitlement = &entitlements[j].ApplicationEntitlementWithVersions
				}
			}
			latest = types.LatestEntitledApplicationVersion(applications[i], entitlement)
			updateAvailable = types.ApplicationUpdateAvailable(applications[i], d.ApplicationVersionID, latest)
		}
		deploymentsByTarget[d.DeploymentTargetID] = append(
			deploymentsByTarget[d.DeploymentTargetID],
			mapping.OverviewDeploymentToAPI(d, latest, updateAvailable),
		)
	}

	result := make([]api.OverviewDeploymentTarget, len(targets))
	for i, target := range targets {
		result[i] = mapping.OverviewDeploymentTargetToAPI(target, deploymentsByTarget[target.ID])
	}
	RespondJSON(w, result)
}

func getOverviewArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	orgID, customerOrgID := *auth.CurrentOrgID(), *auth.CurrentCustomerOrgID()

	artifacts, entitlements, err := visibleArtifacts(ctx)
	if err != nil {
		respondInternalError(w, r, err, "failed to get artifacts")
		return
	}
	artifactIDs := make([]uuid.UUID, len(artifacts))
	for i, a := range artifacts {
		artifactIDs[i] = a.ID
	}
	pulls, err := db.GetOverviewLatestArtifactPulls(ctx, orgID, customerOrgID, artifactIDs)
	if err != nil {
		respondInternalError(w, r, err, "failed to get artifact pulls")
		return
	}

	response := api.OverviewArtifactsResponse{Artifacts: []api.OverviewArtifact{}, HasPulled: len(pulls) > 0}
	for _, artifact := range artifacts {
		if artifact.HiddenInCustomerOverview {
			continue
		}
		latest, err := db.GetNewestArtifactTag(ctx, artifact.ID, entitledArtifactVersionIDs(artifact.ID, entitlements))
		if err != nil {
			respondInternalError(w, r, err, "failed to get newest artifact tag")
			return
		}
		var pull *types.OverviewArtifactPull
		if i := slices.IndexFunc(pulls, func(p types.OverviewArtifactPull) bool {
			return p.ArtifactID == artifact.ID
		}); i >= 0 {
			pull = &pulls[i]
		}
		response.Artifacts = append(response.Artifacts, mapping.OverviewArtifactToAPI(artifact, latest, pull))
	}
	RespondJSON(w, response)
}

// entitledArtifactVersionIDs returns the versions of the artifact the entitlements are limited to, or nil
// when they cover all of them, which GetNewestArtifactTag both expects.
func entitledArtifactVersionIDs(artifactID uuid.UUID, entitlements []types.ArtifactEntitlement) []uuid.UUID {
	var versionIDs []uuid.UUID
	for _, entitlement := range entitlements {
		for _, selection := range entitlement.Artifacts {
			if selection.ArtifactID != artifactID {
				continue
			} else if len(selection.VersionIDs) == 0 {
				return nil
			}
			versionIDs = append(versionIDs, selection.VersionIDs...)
		}
	}
	return versionIDs
}

func getOverviewChecklist(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	if checklist, err := db.GetOverviewChecklist(
		ctx, *auth.CurrentOrgID(), *auth.CurrentCustomerOrgID(), auth.CurrentUserID(),
	); err != nil {
		respondInternalError(w, r, err, "failed to get overview checklist")
	} else {
		RespondJSON(w, mapping.OverviewChecklistToAPI(*checklist))
	}
}

func getOverviewLicense(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	if license, err := db.GetOverviewLicense(ctx, *auth.CurrentOrgID(), *auth.CurrentCustomerOrgID()); err != nil {
		respondInternalError(w, r, err, "failed to get license")
	} else if license == nil {
		w.WriteHeader(http.StatusNoContent)
	} else {
		RespondJSON(w, mapping.OverviewLicenseToAPI(*license))
	}
}

func getOverviewAlerts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	if summary, err := db.GetOverviewAlertSummary(ctx, *auth.CurrentOrgID(), *auth.CurrentCustomerOrgID()); err != nil {
		respondInternalError(w, r, err, "failed to get alert summary")
	} else {
		RespondJSON(w, mapping.OverviewAlertSummaryToAPI(*summary))
	}
}

func getOverviewAccessTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	tokens, err := db.GetAccessTokens(ctx, auth.CurrentUserID(), *auth.CurrentOrgID())
	if err != nil {
		respondInternalError(w, r, err, "failed to get access tokens")
		return
	}
	now := time.Now()
	threshold := now.Add(overviewAccessTokenExpiryWindow)
	result := []api.OverviewAccessToken{}
	for _, token := range tokens {
		if expiresAt := token.NextExpiresAt(now); expiresAt != nil && expiresAt.Before(threshold) {
			result = append(result, mapping.OverviewAccessTokenToAPI(token, *expiresAt))
		}
	}
	slices.SortFunc(result, func(a, b api.OverviewAccessToken) int { return a.ExpiresAt.Compare(b.ExpiresAt) })
	RespondJSON(w, result)
}

func getOverviewSupportBundles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	bundles, err := db.GetSupportBundles(ctx, *auth.CurrentOrgID(), auth.CurrentCustomerOrgID(), nil)
	if err != nil {
		respondInternalError(w, r, err, "failed to get support bundles")
		return
	}
	result := mapping.List(bundles, mapping.OverviewSupportBundleToAPI)
	slices.SortFunc(result, func(a, b api.OverviewSupportBundle) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	RespondJSON(w, result)
}

func getOverviewActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := auth.Authentication.Require(ctx)
	orgID, customerOrgID := *auth.CurrentOrgID(), *auth.CurrentCustomerOrgID()

	entitledApplicationsOnly, entitledArtifactsOnly := false, false
	if auth.CurrentOrg().HasFeature(types.FeatureLicensing) {
		var err error
		if entitledApplicationsOnly, err = db.HasAnyApplicationEntitlement(ctx, orgID); err != nil {
			respondInternalError(w, r, err, "failed to check for application entitlements")
			return
		}
		if entitledArtifactsOnly, err = db.HasAnyArtifactEntitlement(ctx, orgID); err != nil {
			respondInternalError(w, r, err, "failed to check for artifact entitlements")
			return
		}
	}

	if activity, err := db.GetOverviewActivity(
		ctx, orgID, customerOrgID, entitledApplicationsOnly, entitledArtifactsOnly,
	); err != nil {
		respondInternalError(w, r, err, "failed to get overview activity")
	} else {
		RespondJSON(w, mapping.List(activity, mapping.OverviewActivityToAPI))
	}
}
