package api

import (
	"strings"
	"testing"
)

func validResourceReport() ReportSupportBundleResourceRequest {
	return ReportSupportBundleResourceRequest{
		StableKey: "custom-script:11111111-1111-1111-1111-111111111111",
		Name:      "disk usage",
		Status:    SupportBundleResourceStatusSucceeded,
	}
}

func TestReportSupportBundleResourceRequestValidate(t *testing.T) {
	t.Run("accepts a valid report", func(t *testing.T) {
		r := validResourceReport()
		r.ContentSHA256 = strings.Repeat("a", 64)
		r.FailureReason = "ok"
		if err := r.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("defaults status to succeeded", func(t *testing.T) {
		r := validResourceReport()
		r.Status = ""
		if err := r.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
		if r.Status != SupportBundleResourceStatusSucceeded {
			t.Fatalf("expected succeeded default, got %q", r.Status)
		}
	})

	for name, mutate := range map[string]func(*ReportSupportBundleResourceRequest){
		"missing stable key":        func(r *ReportSupportBundleResourceRequest) { r.StableKey = "" },
		"unstable stable key":       func(r *ReportSupportBundleResourceRequest) { r.StableKey = "../escape" },
		"stable key with newline":   func(r *ReportSupportBundleResourceRequest) { r.StableKey = "a\nb" },
		"missing name":              func(r *ReportSupportBundleResourceRequest) { r.Name = "" },
		"name with slash":           func(r *ReportSupportBundleResourceRequest) { r.Name = "a/b" },
		"invalid status":            func(r *ReportSupportBundleResourceRequest) { r.Status = "done" },
		"invalid kind":              func(r *ReportSupportBundleResourceRequest) { r.Kind = "magic" },
		"malformed digest":          func(r *ReportSupportBundleResourceRequest) { r.ContentSHA256 = "abc" },
		"too long failure reason":   func(r *ReportSupportBundleResourceRequest) { r.FailureReason = strings.Repeat("x", maxFailureReasonLength+1) },
	} {
		t.Run(name, func(t *testing.T) {
			r := validResourceReport()
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}

	t.Run("accepts container names and every outcome", func(t *testing.T) {
		for _, status := range []SupportBundleResourceStatus{
			SupportBundleResourceStatusSucceeded,
			SupportBundleResourceStatusSkipped,
			SupportBundleResourceStatusOversized,
			SupportBundleResourceStatusFailed,
		} {
			r := validResourceReport()
			r.StableKey = "web-1-container-logs"
			r.Name = "web-1-container-logs"
			r.Kind = SupportBundleResourceKindContainerLogs
			r.Status = status
			if err := r.Validate(); err != nil {
				t.Fatalf("status %q: expected valid, got %v", status, err)
			}
		}
	})
}
