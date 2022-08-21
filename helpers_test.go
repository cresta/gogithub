package gogithub

import (
	"github.com/joho/godotenv"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func readEnvFile(t *testing.T, rootDir string) {
	expectedEnvPath := filepath.Join(rootDir, ".env")
	if _, err := os.Stat(expectedEnvPath); err != nil {
		return
	}
	envs, err := godotenv.Read(expectedEnvPath)
	require.NoError(t, err)
	for k, v := range envs {
		if os.Getenv(k) == "" {
			t.Setenv(k, v)
		}
	}
}
