//	go run ./scripts/create_tenant -name "Acme Corp"

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/elhassanefek/transacta/internal/tenants"
)

func main() {
	name := flag.String("name", "", "tenant name (required)")
	dsn := flag.String("dsn", "", "Postgres DSN (defaults to DB_* env vars matching cmd/api)")
	flag.Parse()

	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: -name is required")
		os.Exit(1)
	}

	connStr := *dsn
	if connStr == "" {
		connStr = fmt.Sprintf(
			"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
			getenv("DB_HOST", "localhost"), getenv("DB_PORT", "5432"),
			getenv("DB_USER", "transacta"), getenv("DB_PASSWORD", "transacta_dev"),
			getenv("DB_NAME", "transacta"), getenv("DB_SSLMODE", "disable"),
		)
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open database: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	rawKey, err := generateAPIKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: generate API key: %v\n", err)
		os.Exit(1)
	}
	hash := tenants.HashAPIKey(rawKey)

	ctx := context.Background()
	var tenantID string
	err = db.QueryRowContext(ctx,
		`INSERT INTO tenants (name, api_key_hash) VALUES ($1, $2) RETURNING id`,
		*name, hash,
	).Scan(&tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create tenant: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Tenant created.")
	fmt.Printf("  Tenant ID: %s\n", tenantID)
	fmt.Printf("  Name:      %s\n", *name)
	fmt.Println()
	fmt.Println("  API Key (shown once, never stored in plaintext -- save it now):")
	fmt.Printf("  %s\n", rawKey)
}

func generateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk_" + hex.EncodeToString(buf), nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
