// Package config loads Transacta's process configuration from
// environment variables. There are few enough settings (DB connection
// parameters, listen port, JWT signing secret) that a dedicated config
// library isn't warranted -- this is the same six env vars the CI
// workflow's test job already sets, read with os.Getenv and a fallback.
package config

import (
	"fmt"
	"os"
)

// Config holds every environment-derived setting the API process needs
// at startup.
type Config struct {
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string
	Port       string
	JWTSecret  string
}

// Load reads Config from the process environment, applying the same
// local-dev defaults used by docker-compose.yml wherever a variable is
// unset.
func Load() Config {
	return Config{
		DBHost:     getenv("DB_HOST", "localhost"),
		DBPort:     getenv("DB_PORT", "5432"),
		DBUser:     getenv("DB_USER", "transacta"),
		DBPassword: getenv("DB_PASSWORD", "transacta_dev"),
		DBName:     getenv("DB_NAME", "transacta"),
		DBSSLMode:  getenv("DB_SSLMODE", "disable"),
		Port:       getenv("PORT", "8080"),
		JWTSecret:  getenv("JWT_SECRET", ""),
	}
}

// DatabaseDSN builds the libpq-style connection string pgx expects.
func (c Config) DatabaseDSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode,
	)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
