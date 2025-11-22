package config

import "os"

type Config struct {
	WebhookGithubSecret string
	GitHubToken         string
}

func LoadConfig() *Config {
	return &Config{
		WebhookGithubSecret: getEnv("GITHUB_WEBHOOK_SECRET", ""),
		GitHubToken:         getEnv("GITHUB_TOKEN", ""),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
