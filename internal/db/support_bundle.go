package db

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/distr-sh/distr/internal/apierrors"
	internalctx "github.com/distr-sh/distr/internal/context"
	"github.com/distr-sh/distr/internal/dbcrypto"
	"github.com/distr-sh/distr/internal/supportbundle"
	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Configuration

func GetSupportBundleConfigurationEnvVars(
	ctx context.Context, orgID uuid.UUID,
) ([]types.SupportBundleConfigurationEnvVar, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT organization_id, name, redacted
		FROM SupportBundleConfigurationEnvVar
		WHERE organization_id = @orgId
		ORDER BY name`,
		pgx.NamedArgs{"orgId": orgID},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle config env vars: %w", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.SupportBundleConfigurationEnvVar])
	if err != nil {
		return nil, fmt.Errorf("could not get support bundle config env vars: %w", err)
	}
	return result, nil
}

func SaveSupportBundleConfigurationEnvVars(
	ctx context.Context,
	orgID uuid.UUID,
	envVars []types.SupportBundleConfigurationEnvVar,
) error {
	return RunTxRR(ctx, func(ctx context.Context) error {
		db := internalctx.GetDb(ctx)

		if _, err := db.Exec(
			ctx,
			`DELETE FROM SupportBundleConfigurationEnvVar WHERE organization_id = @orgId`,
			pgx.NamedArgs{"orgId": orgID},
		); err != nil {
			return fmt.Errorf("could not delete existing env vars: %w", err)
		}

		if len(envVars) > 0 {
			_, err := db.CopyFrom(
				ctx,
				pgx.Identifier{"supportbundleconfigurationenvvar"},
				[]string{"organization_id", "name", "redacted"},
				pgx.CopyFromSlice(len(envVars), func(i int) ([]any, error) {
					return []any{orgID, envVars[i].Name, envVars[i].Redacted}, nil
				}),
			)
			if err != nil {
				return fmt.Errorf("could not insert env vars: %w", err)
			}
		}

		return nil
	})
}

func ExistsSupportBundleConfigurationEnvVars(ctx context.Context, orgID uuid.UUID) (bool, error) {
	db := internalctx.GetDb(ctx)
	var exists bool
	err := db.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM SupportBundleConfigurationEnvVar WHERE organization_id = @orgId)`,
		pgx.NamedArgs{"orgId": orgID},
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("could not check support bundle config env vars existence: %w", err)
	}
	return exists, nil
}

const supportBundleConfigurationScriptOutputExpr = `
	id, created_at, organization_id, name, description, content, enabled
`

func GetSupportBundleConfigurationScripts(
	ctx context.Context, orgID uuid.UUID,
) ([]types.SupportBundleConfigurationScript, error) {
	return getSupportBundleConfigurationScripts(ctx, orgID, false)
}

func GetEnabledSupportBundleConfigurationScripts(
	ctx context.Context, orgID uuid.UUID,
) ([]types.SupportBundleConfigurationScript, error) {
	return getSupportBundleConfigurationScripts(ctx, orgID, true)
}

func getSupportBundleConfigurationScripts(
	ctx context.Context, orgID uuid.UUID, enabledOnly bool,
) ([]types.SupportBundleConfigurationScript, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		fmt.Sprintf(`SELECT %v
		FROM SupportBundleConfigurationScript
		WHERE organization_id = @orgId AND (NOT @enabledOnly OR enabled)
		ORDER BY name`, supportBundleConfigurationScriptOutputExpr),
		pgx.NamedArgs{"orgId": orgID, "enabledOnly": enabledOnly},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle config scripts: %w", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.SupportBundleConfigurationScript])
	if err != nil {
		return nil, fmt.Errorf("could not get support bundle config scripts: %w", err)
	}
	return result, nil
}

func CreateSupportBundleConfigurationScript(
	ctx context.Context, script *types.SupportBundleConfigurationScript,
) error {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		fmt.Sprintf(`INSERT INTO SupportBundleConfigurationScript
			(organization_id, name, description, content, enabled)
		VALUES (@orgId, @name, @description, @content, @enabled)
		RETURNING %v`, supportBundleConfigurationScriptOutputExpr),
		pgx.NamedArgs{
			"orgId":       script.OrganizationID,
			"name":        script.Name,
			"description": script.Description,
			"content":     script.Content,
			"enabled":     script.Enabled,
		},
	)
	if err != nil {
		return fmt.Errorf("could not create support bundle config script: %w", err)
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundleConfigurationScript])
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgerrcode.UniqueViolation {
		return fmt.Errorf("%w: %w", apierrors.ErrConflict, err)
	} else if err != nil {
		return fmt.Errorf("could not create support bundle config script: %w", err)
	}
	*script = result
	return nil
}

func UpdateSupportBundleConfigurationScript(
	ctx context.Context, script *types.SupportBundleConfigurationScript,
) error {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		fmt.Sprintf(`UPDATE SupportBundleConfigurationScript
		SET name = @name, description = @description, content = @content, enabled = @enabled
		WHERE id = @id AND organization_id = @orgId
		RETURNING %v`, supportBundleConfigurationScriptOutputExpr),
		pgx.NamedArgs{
			"id":          script.ID,
			"orgId":       script.OrganizationID,
			"name":        script.Name,
			"description": script.Description,
			"content":     script.Content,
			"enabled":     script.Enabled,
		},
	)
	if err != nil {
		return fmt.Errorf("could not update support bundle config script: %w", err)
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundleConfigurationScript])
	if errors.Is(err, pgx.ErrNoRows) {
		return apierrors.ErrNotFound
	} else if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == pgerrcode.UniqueViolation {
		return fmt.Errorf("%w: %w", apierrors.ErrConflict, err)
	} else if err != nil {
		return fmt.Errorf("could not update support bundle config script: %w", err)
	}
	*script = result
	return nil
}

func DeleteSupportBundleConfigurationScript(ctx context.Context, id, orgID uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	result, err := db.Exec(
		ctx,
		`DELETE FROM SupportBundleConfigurationScript WHERE id = @id AND organization_id = @orgId`,
		pgx.NamedArgs{"id": id, "orgId": orgID},
	)
	if err != nil {
		return fmt.Errorf("could not delete support bundle config script: %w", err)
	}
	if result.RowsAffected() == 0 {
		return apierrors.ErrNotFound
	}
	return nil
}

// Bundles

var supportBundleWithDetailsOutputExpr = `
	sb.id,
	sb.created_at,
	sb.organization_id,
	sb.customer_organization_id,
	sb.created_by_user_account_id,
	sb.title,
	sb.description,
	sb.status,
	` + supportBundleSecret.Output("sb") + `,
	sb.bundle_secret_expires_at,
	sb.status_changed_by_user_account_id,
	sb.status_changed_at,
	u.name AS created_by_user_name,
	u.image_id AS created_by_image_id,
	co.name AS customer_organization_name,
	(SELECT count(*) FROM SupportBundleResource
		WHERE support_bundle_id = sb.id AND status = 'succeeded') AS resource_count,
	(SELECT count(*) FROM SupportBundleComment WHERE support_bundle_id = sb.id) AS comment_count,
	(SELECT max(created_at) FROM SupportBundleComment WHERE support_bundle_id = sb.id) AS last_comment_at,
	scu.name AS status_changed_by_user_name,
	scu.image_id AS status_changed_by_image_id
`

func GetSupportBundles(
	ctx context.Context, orgID uuid.UUID, customerOrgID *uuid.UUID, partnerOrgID *uuid.UUID,
) ([]types.SupportBundleWithDetails, error) {
	db := internalctx.GetDb(ctx)
	isVendor := customerOrgID == nil && partnerOrgID == nil
	query := fmt.Sprintf(`
		SELECT %v
		FROM SupportBundle sb
		LEFT JOIN UserAccount u ON sb.created_by_user_account_id = u.id
		INNER JOIN CustomerOrganization co ON sb.customer_organization_id = co.id
		LEFT JOIN UserAccount scu ON sb.status_changed_by_user_account_id = scu.id
		WHERE sb.organization_id = @orgId
		AND (@isVendor OR sb.customer_organization_id = @customerOrgId OR co.partner_organization_id = @partnerOrgId)`,
		supportBundleWithDetailsOutputExpr)

	args := pgx.NamedArgs{
		"orgId":         orgID,
		"isVendor":      isVendor,
		"customerOrgId": customerOrgID,
		"partnerOrgId":  partnerOrgID,
	}
	query += ` ORDER BY sb.created_at DESC`

	rows, err := db.Query(ctx, query, args)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundles: %w", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.SupportBundleWithDetails])
	if err != nil {
		return nil, fmt.Errorf("could not get support bundles: %w", err)
	}
	return result, nil
}

func GetSupportBundleByID(ctx context.Context, id, orgID uuid.UUID) (*types.SupportBundleWithDetails, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		fmt.Sprintf(`
			SELECT %v
			FROM SupportBundle sb
			LEFT JOIN UserAccount u ON sb.created_by_user_account_id = u.id
			INNER JOIN CustomerOrganization co ON sb.customer_organization_id = co.id
			LEFT JOIN UserAccount scu ON sb.status_changed_by_user_account_id = scu.id
			WHERE sb.id = @id AND sb.organization_id = @orgId`,
			supportBundleWithDetailsOutputExpr),
		pgx.NamedArgs{"id": id, "orgId": orgID},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle: %w", err)
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundleWithDetails])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierrors.ErrNotFound
		}
		return nil, fmt.Errorf("could not get support bundle: %w", err)
	}
	return &result, nil
}

// GetSupportBundleByBundleSecret matches the secret in Go rather than in the WHERE clause, because
// the stored secret is encrypted with a fresh nonce per write and therefore never equal to a second
// encryption of the same value. The row is narrowed down by its id first, which the caller has.
//
// The lookup deliberately does not filter on expiry or status: after finalize invalidates the
// secret's expiry a response-lost finalize retry still has to authenticate so it can receive the
// original result. The endpoints themselves decide which bundle states they accept.
func GetSupportBundleByBundleSecret(
	ctx context.Context, id uuid.UUID, bundleSecret string,
) (*types.SupportBundle, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT id, created_at, organization_id, customer_organization_id,
			created_by_user_account_id, title, description, status,
			`+supportBundleSecret.Output("sb")+`, bundle_secret_expires_at,
			status_changed_by_user_account_id, status_changed_at
		FROM SupportBundle sb
		WHERE id = @id`,
		pgx.NamedArgs{"id": id},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle: %w", err)
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundle])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierrors.ErrNotFound
		}
		return nil, fmt.Errorf("could not get support bundle: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(result.BundleSecret), []byte(bundleSecret)) != 1 {
		return nil, apierrors.ErrNotFound
	}
	return &result, nil
}

// CreateSupportBundleCollectionParams are the values the collection generation is fixed with at
// bundle creation. Later changes to the organization configuration never reach the bundle.
type CreateSupportBundleCollectionParams struct {
	EnvVars              []types.SupportBundleConfigurationEnvVar
	Scripts              []types.SupportBundleConfigurationScript
	ResourceMaxBytes     int
	LogTailLines         int
	ScriptTimeoutSeconds int
}

// CreateSupportBundleWithCollection inserts the bundle, its fixed collection generation with
// snapshots of the configuration and enabled scripts, and the required resource manifest, all in a
// single transaction: the collect script can only ever see this one generation.
func CreateSupportBundleWithCollection(
	ctx context.Context,
	bundle *types.SupportBundle,
	params CreateSupportBundleCollectionParams,
) (*types.SupportBundleCollection, error) {
	var collection *types.SupportBundleCollection
	err := RunTx(ctx, func(ctx context.Context) error {
		db := internalctx.GetDb(ctx)

		bundleSecretEnc, err := supportBundleSecret.Encrypt(
			bundle.BundleSecret, bundle.CustomerOrganizationID, bundle.OrganizationID)
		if err != nil {
			return fmt.Errorf("could not encrypt support bundle secret: %w", err)
		}
		rows, err := db.Query(
			ctx,
			`INSERT INTO SupportBundle AS sb
				(organization_id, customer_organization_id, created_by_user_account_id,
				title, description, bundle_secret_enc, bundle_secret_expires_at)
			VALUES (@orgId, @customerOrgId, @userId, @title, @description,
				@bundleSecretEnc, @bundleSecretExpiresAt)
			RETURNING id, created_at, organization_id, customer_organization_id,
				created_by_user_account_id, title, description, status,
				`+supportBundleSecret.Output("sb")+`, bundle_secret_expires_at,
				status_changed_by_user_account_id, status_changed_at`,
			pgx.NamedArgs{
				"orgId":                 bundle.OrganizationID,
				"customerOrgId":         bundle.CustomerOrganizationID,
				"userId":                bundle.CreatedByUserAccountID,
				"title":                 bundle.Title,
				"description":           bundle.Description,
				"bundleSecretEnc":       bundleSecretEnc,
				"bundleSecretExpiresAt": bundle.BundleSecretExpiresAt,
			},
		)
		if err != nil {
			return fmt.Errorf("could not create support bundle: %w", err)
		}
		created, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundle])
		if err != nil {
			return fmt.Errorf("could not create support bundle: %w", err)
		}
		*bundle = created

		collectionRows, err := db.Query(
			ctx,
			`INSERT INTO SupportBundleCollection
				(support_bundle_id, resource_max_bytes, log_tail_lines, script_timeout_seconds)
			VALUES (@bundleId, @resourceMaxBytes, @logTailLines, @scriptTimeoutSeconds)
			RETURNING support_bundle_id, collection_generation, resource_max_bytes, log_tail_lines,
				script_timeout_seconds, created_at, published_at,
				finalized_succeeded_count, finalized_skipped_count,
				finalized_oversized_count, finalized_failed_count`,
			pgx.NamedArgs{
				"bundleId":             bundle.ID,
				"resourceMaxBytes":     params.ResourceMaxBytes,
				"logTailLines":         params.LogTailLines,
				"scriptTimeoutSeconds": params.ScriptTimeoutSeconds,
			},
		)
		if err != nil {
			return fmt.Errorf("could not create support bundle collection: %w", err)
		}
		createdCollection, err := pgx.CollectExactlyOneRow(
			collectionRows, pgx.RowToStructByName[types.SupportBundleCollection],
		)
		if err != nil {
			return fmt.Errorf("could not create support bundle collection: %w", err)
		}
		collection = &createdCollection

		for position, envVar := range params.EnvVars {
			if _, err := db.Exec(
				ctx,
				`INSERT INTO SupportBundleCollectionEnvVar (support_bundle_id, name, redacted, position)
				VALUES (@bundleId, @name, @redacted, @position)`,
				pgx.NamedArgs{
					"bundleId": bundle.ID,
					"name":     envVar.Name,
					"redacted": envVar.Redacted,
					"position": position,
				},
			); err != nil {
				return fmt.Errorf("could not snapshot support bundle env var: %w", err)
			}
		}

		for position, script := range params.Scripts {
			if _, err := db.Exec(
				ctx,
				`INSERT INTO SupportBundleCollectionScript
					(id, support_bundle_id, name, description, content, position)
				VALUES (@id, @bundleId, @name, @description, @content, @position)`,
				pgx.NamedArgs{
					"id":          script.ID,
					"bundleId":    bundle.ID,
					"name":        script.Name,
					"description": script.Description,
					"content":     script.Content,
					"position":    position,
				},
			); err != nil {
				return fmt.Errorf("could not snapshot support bundle script: %w", err)
			}
		}

		if err := insertSupportBundleManifestItem(
			ctx, bundle.ID, supportbundle.SystemInfoStableKey, supportbundle.SystemInfoStableKey,
			types.SupportBundleResourceKindSystemInfo,
		); err != nil {
			return err
		}
		if err := insertSupportBundleManifestItem(
			ctx, bundle.ID, supportbundle.HostEnvStableKey, supportbundle.HostEnvStableKey,
			types.SupportBundleResourceKindHostEnv,
		); err != nil {
			return err
		}
		for _, script := range params.Scripts {
			if err := insertSupportBundleManifestItem(
				ctx, bundle.ID,
				supportbundle.CustomScriptStableKey(script.ID), script.Name,
				types.SupportBundleResourceKindCustomScript,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return collection, nil
}

func insertSupportBundleManifestItem(
	ctx context.Context,
	bundleID uuid.UUID,
	stableKey, name string,
	kind types.SupportBundleResourceKind,
) error {
	db := internalctx.GetDb(ctx)
	if _, err := db.Exec(
		ctx,
		`INSERT INTO SupportBundleResource
			(support_bundle_id, stable_key, name, kind, required, status)
		VALUES (@bundleId, @stableKey, @name, @kind, true, 'pending')`,
		pgx.NamedArgs{
			"bundleId":  bundleID,
			"stableKey": stableKey,
			"name":      name,
			"kind":      kind,
		},
	); err != nil {
		return fmt.Errorf("could not create support bundle manifest item: %w", err)
	}
	return nil
}

// Snapshot readers

func GetSupportBundleCollection(
	ctx context.Context, bundleID uuid.UUID,
) (*types.SupportBundleCollection, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT support_bundle_id, collection_generation, resource_max_bytes, log_tail_lines,
			script_timeout_seconds, created_at, published_at,
			finalized_succeeded_count, finalized_skipped_count,
			finalized_oversized_count, finalized_failed_count
		FROM SupportBundleCollection
		WHERE support_bundle_id = @bundleId`,
		pgx.NamedArgs{"bundleId": bundleID},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle collection: %w", err)
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundleCollection])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierrors.ErrNotFound
		}
		return nil, fmt.Errorf("could not get support bundle collection: %w", err)
	}
	return &result, nil
}

func GetSupportBundleCollectionEnvVars(
	ctx context.Context, bundleID uuid.UUID,
) ([]types.SupportBundleCollectionEnvVar, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT id, support_bundle_id, name, redacted, position
		FROM SupportBundleCollectionEnvVar
		WHERE support_bundle_id = @bundleId
		ORDER BY position`,
		pgx.NamedArgs{"bundleId": bundleID},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle collection env vars: %w", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.SupportBundleCollectionEnvVar])
	if err != nil {
		return nil, fmt.Errorf("could not get support bundle collection env vars: %w", err)
	}
	return result, nil
}

func GetSupportBundleCollectionScripts(
	ctx context.Context, bundleID uuid.UUID,
) ([]types.SupportBundleCollectionScript, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT id, support_bundle_id, name, description, content, position
		FROM SupportBundleCollectionScript
		WHERE support_bundle_id = @bundleId
		ORDER BY position`,
		pgx.NamedArgs{"bundleId": bundleID},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle collection scripts: %w", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.SupportBundleCollectionScript])
	if err != nil {
		return nil, fmt.Errorf("could not get support bundle collection scripts: %w", err)
	}
	return result, nil
}

// RenewSupportBundleSecretIfNeeded extends the collection secret while the bundle is actively being
// collected and the current expiry is inside the renewal window, so a long-running or interrupted
// collection near expiry can keep working on the same bundle generation.
func RenewSupportBundleSecretIfNeeded(
	ctx context.Context, bundleID uuid.UUID, ttl, renewBefore time.Duration,
) error {
	db := internalctx.GetDb(ctx)
	if _, err := db.Exec(
		ctx,
		`UPDATE SupportBundle
		SET bundle_secret_expires_at = now() + make_interval(secs => @ttlSeconds)
		WHERE id = @id
			AND status = 'initialized'
			AND bundle_secret_expires_at IS NOT NULL
			AND bundle_secret_expires_at < now() + make_interval(secs => @renewBeforeSeconds)`,
		pgx.NamedArgs{
			"id":                 bundleID,
			"ttlSeconds":         ttl.Seconds(),
			"renewBeforeSeconds": renewBefore.Seconds(),
		},
	); err != nil {
		return fmt.Errorf("could not renew support bundle secret: %w", err)
	}
	return nil
}

// UpdateSupportBundleStatus changes the status only when the current status is one of expected.
// The conditional WHERE is the race-safe half of the state transitions: two concurrent transitions
// cannot both succeed, and a transition cannot overwrite a finalize that committed first.
func UpdateSupportBundleStatus(
	ctx context.Context,
	id, orgID uuid.UUID,
	status types.SupportBundleStatus,
	changedByUserID *uuid.UUID,
	expected ...types.SupportBundleStatus,
) error {
	// pgx has no encoder for a slice of the enum-typed string, so pass text and cast in SQL: the
	// comparison coerces each text element to the enum.
	expectedText := make([]string, len(expected))
	for i, s := range expected {
		expectedText[i] = string(s)
	}
	db := internalctx.GetDb(ctx)
	result, err := db.Exec(
		ctx,
		`UPDATE SupportBundle
		SET status = @status,
			status_changed_by_user_account_id = @changedBy,
			status_changed_at = now()
		WHERE id = @id AND organization_id = @orgId
			AND status = ANY(@expected::support_bundle_status[])`,
		pgx.NamedArgs{
			"id":        id,
			"orgId":     orgID,
			"status":    status,
			"changedBy": changedByUserID,
			"expected":  expectedText,
		},
	)
	if err != nil {
		return fmt.Errorf("could not update support bundle status: %w", err)
	}
	if result.RowsAffected() == 0 {
		exists, err := supportBundleExists(ctx, id, orgID)
		if err != nil {
			return err
		}
		if !exists {
			return apierrors.ErrNotFound
		}
		return apierrors.NewConflict("support bundle is not in a state that allows this transition")
	}
	return nil
}

func supportBundleExists(ctx context.Context, id, orgID uuid.UUID) (bool, error) {
	db := internalctx.GetDb(ctx)
	var exists bool
	if err := db.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM SupportBundle WHERE id = @id AND organization_id = @orgId)`,
		pgx.NamedArgs{"id": id, "orgId": orgID},
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("could not check support bundle existence: %w", err)
	}
	return exists, nil
}

func DeleteSupportBundle(ctx context.Context, id, orgID uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	result, err := db.Exec(
		ctx,
		`DELETE FROM SupportBundle WHERE id = @id AND organization_id = @orgId`,
		pgx.NamedArgs{"id": id, "orgId": orgID},
	)
	if err != nil {
		return fmt.Errorf("could not delete support bundle: %w", err)
	}
	if result.RowsAffected() == 0 {
		return apierrors.ErrNotFound
	}
	return nil
}

func ClearSupportBundleBundleSecret(ctx context.Context, bundleID uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	if _, err := db.Exec(
		ctx,
		`UPDATE SupportBundle
		SET bundle_secret_expires_at = NULL
		WHERE id = @id`,
		pgx.NamedArgs{"id": bundleID},
	); err != nil {
		return fmt.Errorf("could not clear support bundle secret: %w", err)
	}
	return nil
}

// Resources

var supportBundleResourceOutputExpr = `
	r.id, r.created_at, r.updated_at, r.support_bundle_id, r.stable_key, r.name, r.kind,
	r.required, r.status, ` + supportBundleResourceContent.Output("r") + `,
	r.content_sha256, r.size_bytes, r.failure_reason
`

func GetSupportBundleResources(ctx context.Context, bundleID uuid.UUID) ([]types.SupportBundleResource, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT `+supportBundleResourceOutputExpr+`
		FROM SupportBundleResource r
		WHERE support_bundle_id = @bundleId
		ORDER BY r.created_at, r.stable_key`,
		pgx.NamedArgs{"bundleId": bundleID},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle resources: %w", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.SupportBundleResource])
	if err != nil {
		return nil, fmt.Errorf("could not get support bundle resources: %w", err)
	}
	return result, nil
}

// Sentinel errors for the collection endpoints.

var (
	// ErrSupportBundleNotCollecting is returned when the bundle is finalized, canceled or expired and
	// can therefore no longer accept resources.
	ErrSupportBundleNotCollecting = apierrors.NewConflict("support bundle is not accepting resources")
	// ErrSupportBundleNotActive is returned when a collect-script/status request hits a bundle whose
	// collection is no longer active.
	ErrSupportBundleNotActive = apierrors.NewConflict("support bundle collection is not active")
	// ErrSupportBundleResourceFinalized is returned when a different outcome is reported for a
	// resource that already reached succeeded with content.
	ErrSupportBundleResourceFinalized = apierrors.NewConflict(
		"support bundle resource was already uploaded with different content")
	// ErrSupportBundleInvalidState is returned when finalize runs against a non-initialized,
	// non-created bundle.
	ErrSupportBundleInvalidState = apierrors.NewConflict(
		"support bundle can not be finalized in its current state")
	// ErrSupportBundleDigestMismatch is returned when the client-provided digest does not match the
	// received content.
	ErrSupportBundleDigestMismatch = apierrors.NewBadRequest(
		"sha256 does not match the received content")
)

// SupportBundleFinalizeIncompleteError reports required resources that never left pending. It is a
// precondition failure rather than a server error.
type SupportBundleFinalizeIncompleteError struct {
	MissingStableKeys []string
}

func (e *SupportBundleFinalizeIncompleteError) Error() string {
	return fmt.Sprintf("support bundle is missing required resources: %s", strings.Join(e.MissingStableKeys, ", "))
}

type SupportBundleResourceReport struct {
	BundleID      uuid.UUID
	StableKey     string
	Name          string
	Kind          types.SupportBundleResourceKind
	Status        types.SupportBundleResourceStatus
	Content       []byte
	ContentSHA256 string
	FailureReason string
	// SecretTTL/RenewBefore drive the sliding expiry renewal in the same transaction.
	SecretTTL   time.Duration
	RenewBefore time.Duration
}

type SupportBundleResourceReportResult struct {
	Resource types.SupportBundleResource
	Outcome  string
}

// ReportSupportBundleResource records one resource outcome idempotently. The bundle row is locked
// for the whole transaction, so the check against a concurrent finalize and the write commit
// together: a resource that arrives while finalize publishes either lands before the lock and is
// counted, or is rejected because the bundle already left initialized.
func ReportSupportBundleResource(
	ctx context.Context, in SupportBundleResourceReport,
) (*SupportBundleResourceReportResult, error) {
	var result *SupportBundleResourceReportResult
	err := RunTx(ctx, func(ctx context.Context) error {
		db := internalctx.GetDb(ctx)

		var status types.SupportBundleStatus
		var expiresAt *time.Time
		err := db.QueryRow(
			ctx,
			`SELECT status, bundle_secret_expires_at FROM SupportBundle WHERE id = @id FOR UPDATE`,
			pgx.NamedArgs{"id": in.BundleID},
		).Scan(&status, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierrors.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("could not lock support bundle: %w", err)
		}
		if status != types.SupportBundleStatusInitialized || expiresAt == nil || !expiresAt.After(time.Now()) {
			return ErrSupportBundleNotCollecting
		}

		if err := renewSupportBundleSecretLocked(ctx, in.BundleID, expiresAt, in.SecretTTL, in.RenewBefore); err != nil {
			return err
		}

		var maxBytes int
		if err := db.QueryRow(
			ctx,
			`SELECT resource_max_bytes FROM SupportBundleCollection WHERE support_bundle_id = @id`,
			pgx.NamedArgs{"id": in.BundleID},
		).Scan(&maxBytes); err != nil {
			return fmt.Errorf("could not read support bundle collection: %w", err)
		}

		actualDigest, err := verifyResourceDigest(in.Content, in.ContentSHA256)
		if err != nil {
			return err
		}

		effectiveStatus := in.Status
		if int64(len(in.Content)) > int64(maxBytes) {
			effectiveStatus = types.SupportBundleResourceStatusOversized
		}
		failureReason := resourceFailureReason(effectiveStatus, maxBytes, in.FailureReason)

		existing, itemExists, err := lockSupportBundleResource(ctx, in.BundleID, in.StableKey)
		if err != nil {
			return err
		}

		if itemExists {
			if duplicate, err := existingResourceDecision(existing, effectiveStatus, actualDigest); err != nil {
				return err
			} else if duplicate != nil {
				result = duplicate
				return nil
			}
		}

		result, err = storeSupportBundleResourceOutcome(
			ctx, in, itemExists, effectiveStatus, actualDigest, failureReason)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// verifyResourceDigest returns the hex sha256 of the content and, when the client sent one, checks
// the two match case-insensitively.
func verifyResourceDigest(content []byte, clientDigest string) (string, error) {
	actualDigest := ""
	if len(content) > 0 {
		sum := sha256.Sum256(content)
		actualDigest = hex.EncodeToString(sum[:])
	}
	if clientDigest != "" && !strings.EqualFold(clientDigest, actualDigest) {
		return "", ErrSupportBundleDigestMismatch
	}
	return actualDigest, nil
}

func resourceFailureReason(
	status types.SupportBundleResourceStatus, maxBytes int, reason string,
) *string {
	switch {
	case status == types.SupportBundleResourceStatusOversized && reason == "":
		defaultReason := fmt.Sprintf("resource exceeds the limit of %d bytes", maxBytes)
		return &defaultReason
	case reason != "":
		return &reason
	default:
		return nil
	}
}

func lockSupportBundleResource(
	ctx context.Context, bundleID uuid.UUID, stableKey string,
) (types.SupportBundleResource, bool, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT `+supportBundleResourceOutputExpr+`
		FROM SupportBundleResource r
		WHERE support_bundle_id = @bundleId AND stable_key = @stableKey
		FOR UPDATE OF r`,
		pgx.NamedArgs{"bundleId": bundleID, "stableKey": stableKey},
	)
	if err != nil {
		return types.SupportBundleResource{}, false,
			fmt.Errorf("could not query support bundle resource: %w", err)
	}
	existing, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundleResource])
	if err == nil {
		return existing, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return types.SupportBundleResource{}, false, nil
	}
	return types.SupportBundleResource{}, false,
		fmt.Errorf("could not get support bundle resource: %w", err)
}

// existingResourceDecision decides what a report against an already stored item does. A succeeded
// resource is immutable: an identical retry is a content-addressed duplicate, anything else is a
// conflict rather than a silent overwrite. A nil result means the report progresses to storage.
func existingResourceDecision(
	existing types.SupportBundleResource,
	status types.SupportBundleResourceStatus,
	actualDigest string,
) (*SupportBundleResourceReportResult, error) {
	if existing.Status != types.SupportBundleResourceStatusSucceeded {
		return nil, nil
	}
	if status == types.SupportBundleResourceStatusSucceeded &&
		existing.ContentSHA256 != nil && actualDigest != "" &&
		*existing.ContentSHA256 == actualDigest {
		return &SupportBundleResourceReportResult{
			Resource: existing,
			Outcome:  types.SupportBundleResourceOutcomeDuplicate,
		}, nil
	}
	return nil, ErrSupportBundleResourceFinalized
}

func storeSupportBundleResourceOutcome(
	ctx context.Context,
	in SupportBundleResourceReport,
	itemExists bool,
	effectiveStatus types.SupportBundleResourceStatus,
	actualDigest string,
	failureReason *string,
) (*SupportBundleResourceReportResult, error) {
	contentStored := effectiveStatus == types.SupportBundleResourceStatusSucceeded ||
		(effectiveStatus == types.SupportBundleResourceStatusFailed && len(in.Content) > 0)

	var contentEnc any
	if contentStored {
		encrypted, err := supportBundleResourceContent.Encrypt(
			dbcrypto.String(in.Content), in.BundleID)
		if err != nil {
			return nil, fmt.Errorf("could not encrypt support bundle resource: %w", err)
		}
		contentEnc = encrypted
	}

	var digestArg any
	if contentStored {
		digestArg = actualDigest
	}

	var query string
	args := pgx.NamedArgs{
		"bundleId":      in.BundleID,
		"stableKey":     in.StableKey,
		"name":          in.Name,
		"status":        effectiveStatus,
		"contentEnc":    contentEnc,
		"digest":        digestArg,
		"sizeBytes":     int64(len(in.Content)),
		"failureReason": failureReason,
	}
	if !itemExists {
		kind := in.Kind
		if kind == "" {
			kind = inferSupportBundleResourceKind(in.Name)
		}
		args["kind"] = kind
		query = `INSERT INTO SupportBundleResource AS r
			(support_bundle_id, stable_key, name, kind, required, status,
				content_enc, content_sha256, size_bytes, failure_reason)
		VALUES (@bundleId, @stableKey, @name, @kind, false, @status,
			@contentEnc, @digest, @sizeBytes, @failureReason)
		RETURNING ` + supportBundleResourceOutputExpr
	} else {
		query = `UPDATE SupportBundleResource AS r
		SET status = @status,
			content = NULL,
			content_enc = @contentEnc,
			content_sha256 = @digest,
			size_bytes = @sizeBytes,
			failure_reason = @failureReason,
			updated_at = now()
		WHERE support_bundle_id = @bundleId AND stable_key = @stableKey
		RETURNING ` + supportBundleResourceOutputExpr
	}

	db := internalctx.GetDb(ctx)
	outRows, err := db.Query(ctx, query, args)
	if err != nil {
		return nil, fmt.Errorf("could not store support bundle resource: %w", err)
	}
	stored, err := pgx.CollectExactlyOneRow(
		outRows, pgx.RowToStructByName[types.SupportBundleResource])
	if err != nil {
		return nil, fmt.Errorf("could not store support bundle resource: %w", err)
	}
	outcome := types.SupportBundleResourceOutcomeStored
	if effectiveStatus == types.SupportBundleResourceStatusOversized {
		outcome = types.SupportBundleResourceOutcomeOversized
	}
	return &SupportBundleResourceReportResult{Resource: stored, Outcome: outcome}, nil
}

func renewSupportBundleSecretLocked(
	ctx context.Context,
	bundleID uuid.UUID,
	expiresAt *time.Time,
	ttl, renewBefore time.Duration,
) error {
	if ttl <= 0 || renewBefore <= 0 || expiresAt == nil {
		return nil
	}
	if expiresAt.After(time.Now().Add(renewBefore)) {
		return nil
	}
	return RenewSupportBundleSecretIfNeeded(ctx, bundleID, ttl, renewBefore)
}

func inferSupportBundleResourceKind(name string) types.SupportBundleResourceKind {
	switch {
	case strings.HasSuffix(name, "-container-env"):
		return types.SupportBundleResourceKindContainerEnv
	case strings.HasSuffix(name, "-container-logs"):
		return types.SupportBundleResourceKindContainerLogs
	default:
		return types.SupportBundleResourceKindOther
	}
}

// SupportBundleFinalizeResult is the stable finalize outcome used both for the first publish and
// for a replayed finalize after a lost response.
type SupportBundleFinalizeResult struct {
	Collection types.SupportBundleCollection
	Replayed   bool
}

// FinalizeSupportBundle atomically publishes the manifest: it either commits status, published
// manifest and secret invalidation together, or changes nothing. A repeat call after a completed
// publish returns the stored result instead of failing.
func FinalizeSupportBundle(
	ctx context.Context, bundleID uuid.UUID,
) (*SupportBundleFinalizeResult, error) {
	var result *SupportBundleFinalizeResult
	err := RunTx(ctx, func(ctx context.Context) error {
		db := internalctx.GetDb(ctx)

		var status types.SupportBundleStatus
		err := db.QueryRow(
			ctx,
			`SELECT status FROM SupportBundle WHERE id = @id FOR UPDATE`,
			pgx.NamedArgs{"id": bundleID},
		).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierrors.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("could not lock support bundle: %w", err)
		}

		collection, err := GetSupportBundleCollection(ctx, bundleID)
		if err != nil {
			return err
		}

		if status == types.SupportBundleStatusCreated {
			// Idempotent replay: the publish (and secret invalidation) already committed, so return
			// exactly the stored outcome.
			result = &SupportBundleFinalizeResult{Collection: *collection, Replayed: true}
			return nil
		}
		if status != types.SupportBundleStatusInitialized {
			return ErrSupportBundleInvalidState
		}

		rows, err := db.Query(
			ctx,
			`SELECT stable_key FROM SupportBundleResource
			WHERE support_bundle_id = @bundleId AND required AND status = 'pending'
			ORDER BY stable_key`,
			pgx.NamedArgs{"bundleId": bundleID},
		)
		if err != nil {
			return fmt.Errorf("could not query pending manifest items: %w", err)
		}
		missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("could not read pending manifest items: %w", err)
		}
		if len(missing) > 0 {
			return &SupportBundleFinalizeIncompleteError{MissingStableKeys: missing}
		}

		var counts struct {
			Succeeded int
			Skipped   int
			Oversized int
			Failed    int
			Total     int
		}
		countRows, err := db.Query(
			ctx,
			`SELECT status, count(*)::int AS count
			FROM SupportBundleResource
			WHERE support_bundle_id = @bundleId
			GROUP BY status`,
			pgx.NamedArgs{"bundleId": bundleID},
		)
		if err != nil {
			return fmt.Errorf("could not count manifest items: %w", err)
		}
		type statusCount struct {
			Status types.SupportBundleResourceStatus `db:"status"`
			Count  int                               `db:"count"`
		}
		statusCounts, err := pgx.CollectRows(countRows, pgx.RowToStructByName[statusCount])
		if err != nil {
			return fmt.Errorf("could not read manifest counts: %w", err)
		}
		for _, sc := range statusCounts {
			counts.Total += sc.Count
			switch sc.Status {
			case types.SupportBundleResourceStatusSucceeded:
				counts.Succeeded = sc.Count
			case types.SupportBundleResourceStatusSkipped:
				counts.Skipped = sc.Count
			case types.SupportBundleResourceStatusOversized:
				counts.Oversized = sc.Count
			case types.SupportBundleResourceStatusFailed:
				counts.Failed = sc.Count
			}
		}

		if _, err := db.Exec(
			ctx,
			`UPDATE SupportBundle
			SET status = 'created',
				status_changed_by_user_account_id = NULL,
				status_changed_at = now(),
				bundle_secret_expires_at = NULL
			WHERE id = @id`,
			pgx.NamedArgs{"id": bundleID},
		); err != nil {
			return fmt.Errorf("could not publish support bundle: %w", err)
		}

		updatedCollectionRows, err := db.Query(
			ctx,
			`UPDATE SupportBundleCollection
			SET published_at = now(),
				finalized_succeeded_count = @succeeded,
				finalized_skipped_count = @skipped,
				finalized_oversized_count = @oversized,
				finalized_failed_count = @failed
			WHERE support_bundle_id = @bundleId
			RETURNING support_bundle_id, collection_generation, resource_max_bytes, log_tail_lines,
				script_timeout_seconds, created_at, published_at,
				finalized_succeeded_count, finalized_skipped_count,
				finalized_oversized_count, finalized_failed_count`,
			pgx.NamedArgs{
				"bundleId":  bundleID,
				"succeeded": counts.Succeeded,
				"skipped":   counts.Skipped,
				"oversized": counts.Oversized,
				"failed":    counts.Failed,
			},
		)
		if err != nil {
			return fmt.Errorf("could not publish support bundle manifest: %w", err)
		}
		updatedCollection, err := pgx.CollectExactlyOneRow(
			updatedCollectionRows, pgx.RowToStructByName[types.SupportBundleCollection])
		if err != nil {
			return fmt.Errorf("could not read published support bundle collection: %w", err)
		}
		result = &SupportBundleFinalizeResult{Collection: updatedCollection}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CleanupSupportBundles deletes only unpublished, unrecoverable bundles:
//   - canceled bundles (explicitly abandoned, never published),
//   - initialized bundles whose collection secret expired before the cutoff (the collection can no
//     longer be resumed and no manifest was ever published).
//
// Published bundles (created/resolved) are never deleted here. Resources, snapshots and comments
// are removed by the foreign key cascades.
func CleanupSupportBundles(ctx context.Context, unrecoverableBefore time.Time) (int64, error) {
	db := internalctx.GetDb(ctx)
	result, err := db.Exec(
		ctx,
		`DELETE FROM SupportBundle
		WHERE (status = 'canceled' AND status_changed_at < @cutoff)
		   OR (status = 'initialized'
		       AND (bundle_secret_expires_at IS NULL
		            OR bundle_secret_expires_at < @cutoff))`,
		pgx.NamedArgs{"cutoff": unrecoverableBefore},
	)
	if err != nil {
		return 0, fmt.Errorf("could not clean up support bundles: %w", err)
	}
	return result.RowsAffected(), nil
}

// Comments

func GetSupportBundleComments(ctx context.Context, bundleID uuid.UUID) ([]types.SupportBundleCommentWithUser, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`SELECT c.id, c.created_at, c.support_bundle_id, c.user_account_id, c.content,
			u.name AS user_name, u.image_id AS user_image_id
		FROM SupportBundleComment c
		LEFT JOIN UserAccount u ON c.user_account_id = u.id
		WHERE c.support_bundle_id = @bundleId
		ORDER BY c.created_at`,
		pgx.NamedArgs{"bundleId": bundleID},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query support bundle comments: %w", err)
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[types.SupportBundleCommentWithUser])
	if err != nil {
		return nil, fmt.Errorf("could not get support bundle comments: %w", err)
	}
	return result, nil
}

func CreateSupportBundleComment(
	ctx context.Context, bundleID, userID uuid.UUID, content string,
) (*types.SupportBundleCommentWithUser, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(
		ctx,
		`WITH inserted AS (
			INSERT INTO SupportBundleComment (support_bundle_id, user_account_id, content)
			VALUES (@bundleId, @userId, @content)
			RETURNING *
		)
		SELECT i.id, i.created_at, i.support_bundle_id, i.user_account_id, i.content,
			u.name AS user_name, u.image_id AS user_image_id
		FROM inserted i
		LEFT JOIN UserAccount u ON i.user_account_id = u.id`,
		pgx.NamedArgs{
			"bundleId": bundleID,
			"userId":   userID,
			"content":  content,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("could not create support bundle comment: %w", err)
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[types.SupportBundleCommentWithUser])
	if err != nil {
		return nil, fmt.Errorf("could not create support bundle comment: %w", err)
	}
	return &result, nil
}
