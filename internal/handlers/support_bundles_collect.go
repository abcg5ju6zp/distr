package handlers

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/distr-sh/distr/api"
	"github.com/distr-sh/distr/internal/apierrors"
	"github.com/distr-sh/distr/internal/auth"
	internalctx "github.com/distr-sh/distr/internal/context"
	"github.com/distr-sh/distr/internal/customdomains"
	"github.com/distr-sh/distr/internal/db"
	"github.com/distr-sh/distr/internal/env"
	"github.com/distr-sh/distr/internal/mapping"
	"github.com/distr-sh/distr/internal/supportbundle"
	"github.com/distr-sh/distr/internal/types"
	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"
	"github.com/oaswrap/spec/adapter/chiopenapi"
	"github.com/oaswrap/spec/option"
	"go.uber.org/zap"
)

// SupportBundleScriptRouter handles endpoints called by the collect script.
// All endpoints use query-param token auth tied to the specific bundle. The authenticator accepts a
// valid secret even after completion, so each handler decides which bundle states it serves:
// collection endpoints require an active collection, finalize additionally answers replays.
func SupportBundleScriptRouter(r chiopenapi.Router) {
	r.WithOptions(option.GroupTags("Support Bundles"))

	r.Route("/{bundleId}", func(r chiopenapi.Router) {
		r.Use(auth.SupportBundleAuthentication.Middleware)

		type BundleIDRequest struct {
			BundleID uuid.UUID `path:"bundleId"`
		}

		r.Get("/collect-script", getCollectScriptHandler()).
			With(option.Description("Get support bundle collect script")).
			With(option.Request(BundleIDRequest{})).
			With(option.Response(http.StatusOK, nil, option.ContentType("text/plain")))

		r.Get("/status", getCollectionStatusHandler()).
			With(option.Description("Get the collection resume state")).
			With(option.Request(BundleIDRequest{})).
			With(option.Response(http.StatusOK, nil, option.ContentType("text/plain")))

		r.Post("/resources", reportSupportBundleResourceHandler()).
			With(option.Description("Report a support bundle resource outcome")).
			With(option.Request(struct {
				BundleIDRequest
				StableKey     string `formData:"stableKey"`
				Name          string `formData:"name"`
				Kind          string `formData:"kind"`
				Status        string `formData:"status"`
				Content       string `formData:"content"`
				ContentSHA256 string `formData:"sha256"`
				FailureReason string `formData:"failureReason"`
			}{}, option.ContentType("multipart/form-data"))).
			With(option.Response(http.StatusOK, api.ReportSupportBundleResourceResponse{}))

		r.Post("/finalize", finalizeSupportBundleHandler()).
			With(option.Description("Finalize a support bundle")).
			With(option.Request(BundleIDRequest{})).
			With(option.Response(http.StatusOK, api.FinalizeSupportBundleResponse{}))
	})
}

// collectBundleIsActive reports whether the bundle's collection can still be driven by the script.
func collectBundleIsActive(bundle *types.SupportBundle) bool {
	return bundle.Status == types.SupportBundleStatusInitialized &&
		bundle.BundleSecretExpiresAt != nil &&
		bundle.BundleSecretExpiresAt.After(time.Now())
}

// requireActiveCollectBundle writes a 409 and returns false when the bundle can no longer be
// collected (finalized, canceled or expired).
func requireActiveCollectBundle(w http.ResponseWriter, bundle *types.SupportBundle) bool {
	if collectBundleIsActive(bundle) {
		return true
	}
	http.Error(w, "support bundle collection is no longer active", http.StatusConflict)
	return false
}

func getCollectScriptHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		bundle := auth.SupportBundleAuthentication.Require(ctx)

		if !requireActiveCollectBundle(w, bundle) {
			return
		}

		bundleSecret := r.URL.Query().Get("bundleSecret")

		org, err := db.GetOrganizationWithBranding(ctx, bundle.OrganizationID)
		if err != nil {
			log.Error("failed to get organization", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		baseURL := customdomains.AppDomainOrDefault(ctx, org.ID, org.Branding)

		collection, err := db.GetSupportBundleCollection(ctx, bundle.ID)
		if err != nil {
			log.Error("failed to get support bundle collection", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		envVars, err := db.GetSupportBundleCollectionEnvVars(ctx, bundle.ID)
		if err != nil {
			log.Error("failed to get support bundle collection env vars", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		scripts, err := db.GetSupportBundleCollectionScripts(ctx, bundle.ID)
		if err != nil {
			log.Error("failed to get support bundle collection scripts", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Downloading the script is proof the collection is still being worked on; slide the secret
		// expiry forward inside the renewal window so a slow/interrupted collection near expiry keeps
		// using the same bundle generation.
		if err := db.RenewSupportBundleSecretIfNeeded(
			ctx, bundle.ID, env.SupportBundleSecretTTL(), env.SupportBundleSecretRenewBefore(),
		); err != nil {
			log.Error("failed to renew support bundle secret", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		script, err := supportbundle.GenerateCollectScript(supportbundle.CollectScriptParams{
			BaseURL:              baseURL,
			BundleID:             bundle.ID,
			BundleSecret:         bundleSecret,
			CollectionGeneration: collection.CollectionGeneration,
			EnvVars:              envVars,
			Scripts:              scripts,
			LogTailLines:         collection.LogTailLines,
			ResourceMaxBytes:     collection.ResourceMaxBytes,
			ScriptTimeout:        time.Duration(collection.ScriptTimeoutSeconds) * time.Second,
		})
		if err != nil {
			log.Error("failed to generate collect script", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte(script)); err != nil {
			log.Warn("failed to write collect script", zap.Error(err))
		}
	}
}

func getCollectionStatusHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		bundle := auth.SupportBundleAuthentication.Require(ctx)

		if !requireActiveCollectBundle(w, bundle) {
			return
		}

		// Fetching the resume state is the first call of a continued collection; slide the secret
		// expiry like the other collecting endpoints do.
		if err := db.RenewSupportBundleSecretIfNeeded(
			ctx, bundle.ID, env.SupportBundleSecretTTL(), env.SupportBundleSecretRenewBefore(),
		); err != nil {
			log.Error("failed to renew support bundle secret", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		resources, err := db.GetSupportBundleResources(ctx, bundle.ID)
		if err != nil {
			log.Error("failed to get support bundle resources", zap.Error(err))
			sentry.GetHubFromContext(ctx).CaptureException(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var builder strings.Builder
		for _, resource := range resources {
			// Pending manifest items carry nothing to resume; only decided outcomes are reported.
			if resource.Status == types.SupportBundleResourceStatusPending {
				continue
			}
			builder.WriteString(resource.StableKey)
			builder.WriteByte('\t')
			builder.WriteString(string(resource.Status))
			builder.WriteByte('\t')
			if resource.ContentSHA256 != nil {
				builder.WriteString(*resource.ContentSHA256)
			}
			builder.WriteByte('\n')
		}
		if _, err := w.Write([]byte(builder.String())); err != nil {
			log.Warn("failed to write collection status", zap.Error(err))
		}
	}
}

func reportSupportBundleResourceHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		bundle := auth.SupportBundleAuthentication.Require(ctx)

		if !requireActiveCollectBundle(w, bundle) {
			return
		}

		// The request body size is limited by the requestSize middleware on the route.
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		// Multipart form values do not go through JsonBody, so they are not trimmed generically.
		request := api.ReportSupportBundleResourceRequest{
			StableKey:     strings.TrimSpace(r.FormValue("stableKey")),
			Name:          strings.TrimSpace(r.FormValue("name")),
			Kind:          api.SupportBundleResourceKind(strings.TrimSpace(r.FormValue("kind"))),
			Status:        api.SupportBundleResourceStatus(strings.TrimSpace(r.FormValue("status"))),
			ContentSHA256: strings.TrimSpace(r.FormValue("sha256")),
			FailureReason: strings.TrimSpace(r.FormValue("failureReason")),
		}
		if err := request.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var content []byte
		file, _, fileErr := r.FormFile("content")
		if fileErr == nil {
			defer file.Close()
			content, fileErr = io.ReadAll(file)
			if fileErr != nil {
				http.Error(w, "failed to read content", http.StatusBadRequest)
				return
			}
		} else if !errors.Is(fileErr, http.ErrMissingFile) {
			http.Error(w, "invalid content file", http.StatusBadRequest)
			return
		}

		switch request.Status {
		case api.SupportBundleResourceStatusSucceeded:
			if len(content) == 0 {
				http.Error(w, "content file is required for a succeeded resource", http.StatusBadRequest)
				return
			}
		case api.SupportBundleResourceStatusSkipped, api.SupportBundleResourceStatusOversized:
			// Skipped and oversized outcomes carry a reason, never the bytes.
			content = nil
		}

		result, err := db.ReportSupportBundleResource(ctx, db.SupportBundleResourceReport{
			BundleID:      bundle.ID,
			StableKey:     request.StableKey,
			Name:          request.Name,
			Kind:          types.SupportBundleResourceKind(request.Kind),
			Status:        types.SupportBundleResourceStatus(request.Status),
			Content:       content,
			ContentSHA256: request.ContentSHA256,
			FailureReason: request.FailureReason,
			SecretTTL:     env.SupportBundleSecretTTL(),
			RenewBefore:   env.SupportBundleSecretRenewBefore(),
		})
		if err != nil {
			respondCollectError(w, r, err, "failed to report support bundle resource")
			return
		}

		RespondJSON(w, mapping.SupportBundleResourceReportToAPI(result.Resource, result.Outcome))
	}
}

func finalizeSupportBundleHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := internalctx.GetLogger(ctx)
		bundle := auth.SupportBundleAuthentication.Require(ctx)

		// A replay of an already finalized bundle returns the stored result; an expired or canceled
		// collection can neither be published nor replayed.
		if bundle.Status != types.SupportBundleStatusCreated && !collectBundleIsActive(bundle) {
			http.Error(w, "support bundle can not be finalized in its current state", http.StatusConflict)
			return
		}

		result, err := db.FinalizeSupportBundle(ctx, bundle.ID)
		if err != nil {
			var incomplete *db.SupportBundleFinalizeIncompleteError
			if errors.As(err, &incomplete) {
				RespondJSONWithStatus(w, http.StatusConflict, api.FinalizeSupportBundleConflictResponse{
					Error:            "support bundle has required resources that are not finalized yet",
					MissingResources: incomplete.MissingStableKeys,
				})
				return
			}
			respondCollectError(w, r, err, "failed to finalize support bundle")
			return
		}

		publishedAt := result.Collection.PublishedAt
		if publishedAt == nil {
			log.Error("finalized support bundle has no published_at")
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		RespondJSON(w, api.FinalizeSupportBundleResponse{
			Status:               string(types.SupportBundleStatusCreated),
			CollectionGeneration: result.Collection.CollectionGeneration,
			PublishedAt:          *publishedAt,
			Replayed:             result.Replayed,
			Resources: api.SupportBundleResourceCounts{
				Total: result.Collection.FinalizedSucceededCount +
					result.Collection.FinalizedSkippedCount +
					result.Collection.FinalizedOversizedCount +
					result.Collection.FinalizedFailedCount,
				Succeeded: result.Collection.FinalizedSucceededCount,
				Skipped:   result.Collection.FinalizedSkippedCount,
				Oversized: result.Collection.FinalizedOversizedCount,
				Failed:    result.Collection.FinalizedFailedCount,
			},
		})
	}
}

// respondCollectError maps the db sentinel errors onto collection endpoint status codes and keeps
// unexpected errors in the logs and Sentry.
func respondCollectError(w http.ResponseWriter, r *http.Request, err error, message string) {
	ctx := r.Context()
	switch {
	case errors.Is(err, apierrors.ErrBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, apierrors.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, apierrors.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		internalctx.GetLogger(ctx).Error(message, zap.Error(err))
		sentry.GetHubFromContext(ctx).CaptureException(err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}
