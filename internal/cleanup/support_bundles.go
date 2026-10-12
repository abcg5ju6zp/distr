package cleanup

import (
	"context"
	"time"

	internalctx "github.com/distr-sh/distr/internal/context"
	"github.com/distr-sh/distr/internal/db"
	"github.com/distr-sh/distr/internal/env"
	"go.uber.org/zap"
)

// RunSupportBundleCleanup deletes support bundles that were never published and can no longer be
// recovered: canceled bundles and initialized bundles whose collection secret expired past the
// cleanup min age. Published bundles are left for the normal retention/delete flow.
func RunSupportBundleCleanup(ctx context.Context) error {
	log := internalctx.GetLogger(ctx)
	cutoff := time.Now().Add(-env.CleanupSupportBundleMinAge())
	if count, err := db.CleanupSupportBundles(ctx, cutoff); err != nil {
		return err
	} else {
		log.Info("SupportBundle cleanup finished", zap.Int64("bundlesDeleted", count))
		return nil
	}
}
