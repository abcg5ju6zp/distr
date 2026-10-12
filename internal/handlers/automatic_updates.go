package handlers

import (
	"context"
	"net/http"

	"github.com/distr-sh/distr/internal/autoupdate"
	internalctx "github.com/distr-sh/distr/internal/context"
	"github.com/distr-sh/distr/internal/types"
	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// automaticUpdatesAllowed reports whether the organization and the application currently permit
// automatic updates. A deployment can outlive all three conditions, since the vendor may withdraw
// them from the application and the organization may lose the feature with its plan.
func automaticUpdatesAllowed(org *types.Organization, application *types.Application) bool {
	return autoupdate.Allowed(org, application)
}

// triggerAutomaticApplicationUpdates starts a persistent automatic-update batch for every
// deployment of the application that has automatic updates enabled, rolling it forward to the
// newest version it is entitled to.
//
// The upgrade only ever moves a deployment forward, which is why archiving a version and
// narrowing an entitlement need no special case: both can only lower the newest version, and the
// deployment is then already ahead of it. That also makes the function safe to call after any
// change to an application, its versions or an entitlement.
//
// The caller has to have written its own change to the application before calling, and to be
// inside a transaction. Per-deployment failures are recorded on the batch and never roll the
// trigger back; only persisting the batch itself returns an error.
func triggerAutomaticApplicationUpdates(
	ctx context.Context,
	org *types.Organization,
	applicationID uuid.UUID,
	createdByUserID *uuid.UUID,
) error {
	return autoupdate.Trigger(ctx, org, applicationID, nil, createdByUserID)
}

// triggerAutomaticUpdateOfDeployment is triggerAutomaticApplicationUpdates for the single
// deployment automatic updates have just been enabled on.
func triggerAutomaticUpdateOfDeployment(
	ctx context.Context,
	org *types.Organization,
	applicationID uuid.UUID,
	deploymentID uuid.UUID,
	createdByUserID *uuid.UUID,
) error {
	return autoupdate.Trigger(ctx, org, applicationID, &deploymentID, createdByUserID)
}

// automaticUpdateError writes the response for a failed automatic update, for a handler whose
// transaction only rolls back on the error it returns.
func automaticUpdateError(ctx context.Context, w http.ResponseWriter, err error) error {
	internalctx.GetLogger(ctx).Warn("could not trigger automatic application updates", zap.Error(err))
	sentry.GetHubFromContext(ctx).CaptureException(err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	return err
}
