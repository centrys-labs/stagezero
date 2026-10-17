package config

import (
	"strings"
	"testing"
	"time"
)

func TestSourceAnalysisLimitsEnvironment(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", strings.Repeat("0", 64))
	t.Setenv("SOURCE_ANALYSIS_REQUEST_BYTES", "8192")
	t.Setenv("SOURCE_ANALYSIS_TARBALL_BYTES", "1024")
	t.Setenv("SOURCE_ANALYSIS_EXTRACTED_BYTES", "4096")
	t.Setenv("SOURCE_ANALYSIS_OUTPUT_BYTES", "512")
	t.Setenv("SOURCE_ANALYSIS_ENTRIES", "42")
	t.Setenv("SOURCE_ANALYSIS_RATE_PER_MINUTE", "3")
	t.Setenv("SOURCE_ANALYSIS_CONCURRENCY", "1")
	t.Setenv("SOURCE_ANALYSIS_TIMEOUT_SECONDS", "10")
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	got := C.SourceAnalysis
	if got.RequestBytes != 8192 || got.TarballBytes != 1024 || got.ExtractedBytes != 4096 || got.OutputBytes != 512 || got.Entries != 42 || got.RatePerMinute != 3 || got.Concurrency != 1 || got.Timeout != 10*time.Second {
		t.Fatalf("source analysis limits = %+v", got)
	}
	for _, key := range []string{"SOURCE_ANALYSIS_REQUEST_BYTES", "SOURCE_ANALYSIS_TARBALL_BYTES", "SOURCE_ANALYSIS_EXTRACTED_BYTES", "SOURCE_ANALYSIS_OUTPUT_BYTES", "SOURCE_ANALYSIS_ENTRIES", "SOURCE_ANALYSIS_RATE_PER_MINUTE", "SOURCE_ANALYSIS_CONCURRENCY", "SOURCE_ANALYSIS_TIMEOUT_SECONDS"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "0")
			if err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}
