package config

import (
	"os"
	"strconv"
)

type Config struct {
	WebhookGithubSecret  string
	GitHubToken          string
	GitHubAppID          int64
	GitHubInstallationID int64
	GitHubPrivateKeyPath string
}

func LoadConfig() *Config {
	return &Config{
		WebhookGithubSecret:  getEnv("GITHUB_WEBHOOK_SECRET", ""),
		GitHubToken:          getEnv("GITHUB_TOKEN", ""),
		GitHubAppID:          getEnvAsInt64("GITHUB_APP_ID", 0),
		GitHubInstallationID: getEnvAsInt64("GITHUB_INSTALLATION_ID", 0),
		GitHubPrivateKeyPath: getEnv("GITHUB_PRIVATE_KEY_PATH", "/path/to/private-key.pem"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.ParseInt(value, 10, 64); err == nil {
			return intValue
		}
	}
	return defaultValue
}
