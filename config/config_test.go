package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealthDefaults locks the embedded default.config.yaml health block,
// including that the "30m" string decodes into a time.Duration (this repo's
// first duration config field).
func TestHealthDefaults(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, cfg.Health.SyncMaxStaleness)
	assert.Equal(t, 10000, cfg.Health.MaxRawPending)
	assert.Equal(t, 100, cfg.Health.MaxAttachmentFailures)
}

// TestHealthEnvDurationOverride locks the env path for the duration field —
// MLS_SYNC_HEALTH_SYNC_MAX_STALENESS=1s must decode to time.Second, not just
// the YAML default.
func TestHealthEnvDurationOverride(t *testing.T) {
	t.Setenv("MLS_SYNC_HEALTH_SYNC_MAX_STALENESS", "1s")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, time.Second, cfg.Health.SyncMaxStaleness)
}
