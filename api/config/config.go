package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/centrys-labs/stagezero/crypto"
	"github.com/centrys-labs/stagezero/source"
)

var C Config

type Config struct {
	DatabaseURL        string
	EncryptionKey      string
	GitHubClientID     string
	GitHubClientSecret string
	GoogleClientID     string
	GoogleClientSecret string
	OAuthRedirectBase  string
	CookieSecure       bool
	SourceAnalysis     source.AnalysisLimits
}

func Load() error {
	C = Config{
		DatabaseURL:        envDefault("DATABASE_URL", "postgres://stagezero:password@localhost:5432/stagezero"),
		EncryptionKey:      os.Getenv("ENCRYPTION_KEY"),
		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		OAuthRedirectBase:  os.Getenv("OAUTH_REDIRECT_BASE_URL"),
		CookieSecure:       os.Getenv("COOKIE_SECURE") != "false",
		SourceAnalysis:     source.DefaultAnalysisLimits(),
	}
	for _, setting := range []struct {
		name  string
		value *int64
	}{
		{"SOURCE_ANALYSIS_REQUEST_BYTES", &C.SourceAnalysis.RequestBytes},
		{"SOURCE_ANALYSIS_TARBALL_BYTES", &C.SourceAnalysis.TarballBytes},
		{"SOURCE_ANALYSIS_EXTRACTED_BYTES", &C.SourceAnalysis.ExtractedBytes},
		{"SOURCE_ANALYSIS_OUTPUT_BYTES", &C.SourceAnalysis.OutputBytes},
	} {
		if err := positiveEnv(setting.name, setting.value); err != nil {
			return err
		}
	}
	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"SOURCE_ANALYSIS_ENTRIES", &C.SourceAnalysis.Entries},
		{"SOURCE_ANALYSIS_RATE_PER_MINUTE", &C.SourceAnalysis.RatePerMinute},
		{"SOURCE_ANALYSIS_CONCURRENCY", &C.SourceAnalysis.Concurrency},
	} {
		value := int64(*setting.value)
		if err := positiveEnv(setting.name, &value); err != nil {
			return err
		}
		if int64(int(value)) != value {
			return fmt.Errorf("%s is too large", setting.name)
		}
		*setting.value = int(value)
	}
	seconds := int64(C.SourceAnalysis.Timeout / time.Second)
	if err := positiveEnv("SOURCE_ANALYSIS_TIMEOUT_SECONDS", &seconds); err != nil {
		return err
	}
	if seconds > int64((1<<63-1)/int64(time.Second)) {
		return fmt.Errorf("SOURCE_ANALYSIS_TIMEOUT_SECONDS is too large")
	}
	C.SourceAnalysis.Timeout = time.Duration(seconds) * time.Second

	if (C.GitHubClientID == "") != (C.GitHubClientSecret == "") {
		return fmt.Errorf("GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET must both be set")
	}
	if (C.GoogleClientID == "") != (C.GoogleClientSecret == "") {
		return fmt.Errorf("GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET must both be set")
	}

	hasProvider := C.GitHubClientID != "" || C.GoogleClientID != ""
	if hasProvider && C.OAuthRedirectBase == "" {
		return fmt.Errorf("OAUTH_REDIRECT_BASE_URL is required when OAuth providers are configured")
	}

	if C.EncryptionKey == "" {
		return fmt.Errorf("ENCRYPTION_KEY is required (64 hex chars for AES-256)")
	}
	if err := crypto.Init(C.EncryptionKey); err != nil {
		return err
	}

	return nil
}

func positiveEnv(key string, value *int64) error {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n == 1<<63-1 {
			return fmt.Errorf("%s must be a positive integer", key)
		}
		*value = n
	}
	return nil
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
