package main

import (
	"github.com/distr-sh/distr/internal/controllerapply"
	"github.com/distr-sh/distr/internal/envutil"
)

func ScratchDir() string {
	if dir := envutil.GetEnv("DISTR_CONTROLLER_SCRATCH_DIR"); dir != "" {
		return dir
	}
	return "./scratch"
}

// deploymentStore owns the atomic state, application generations and engine receipts under ScratchDir.
var deploymentStore = controllerapply.New(ScratchDir())
