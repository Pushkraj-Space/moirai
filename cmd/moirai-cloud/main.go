package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/october-dev/moirai/internal/cloud"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("cloud stopped", "error", err)
		os.Exit(1)
	}
}
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dev := os.Getenv("MOIRAI_DEVELOPMENT") == "true"
	origin := env("MOIRAI_ORIGIN", "http://127.0.0.1:8080")
	driver := env("MOIRAI_DB_DRIVER", "pgx")
	dsn := os.Getenv("DATABASE_URL")
	if dev && dsn == "" {
		driver = "sqlite"
		if err := os.MkdirAll(".cloud", 0700); err != nil {
			return err
		}
		dsn = ".cloud/cloud.db"
	}
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	if !dev && driver != "pgx" {
		return errors.New("production requires Postgres")
	}
	db, err := cloud.OpenDB(ctx, driver, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = cloud.Migrate(ctx, db, driver); err != nil {
		return err
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		fmt.Println("Schema is current.")
		return nil
	}
	key, err := hex.DecodeString(os.Getenv("MOIRAI_ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		return errors.New("MOIRAI_ENCRYPTION_KEY must be 64 hex characters (generate with openssl rand -hex 32)")
	}
	var base cloud.Blobs
	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		secure := os.Getenv("S3_INSECURE") != "true"
		if !secure && !dev {
			return errors.New("production S3 requires TLS")
		}
		if os.Getenv("S3_BUCKET") == "" || os.Getenv("S3_ACCESS_KEY") == "" || os.Getenv("S3_SECRET_KEY") == "" {
			return errors.New("S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are required")
		}
		base, err = cloud.NewS3(endpoint, os.Getenv("S3_BUCKET"), os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), secure)
		if err != nil {
			return err
		}
	} else if dev {
		root, err := filepath.Abs(env("MOIRAI_BLOB_DIR", ".cloud/blobs"))
		if err != nil {
			return err
		}
		base = cloud.Files{Root: root}
	} else {
		return errors.New("S3_ENDPOINT is required in production")
	}
	blobs, err := cloud.EncryptBlobs(base, key)
	if err != nil {
		return err
	}
	var trusted []string
	if raw := os.Getenv("MOIRAI_TRUSTED_PROXY_CIDRS"); raw != "" {
		trusted = strings.Split(raw, ",")
	}
	server, err := cloud.New(db, blobs, cloud.Config{Origin: origin, Development: dev, GitHubClientID: os.Getenv("GITHUB_CLIENT_ID"), GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"), SupportEmail: env("MOIRAI_SUPPORT_EMAIL", "Contact the deployment operator; support address is not configured."), TrustedProxyCIDRs: trusted, MetricsToken: os.Getenv("MOIRAI_METRICS_TOKEN")})
	if err != nil {
		return err
	}
	address := env("MOIRAI_ADDR", "127.0.0.1:8080")
	httpServer := &http.Server{Addr: address, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() {
		slog.Info("cloud listening", "address", address, "development", dev)
		done <- httpServer.ListenAndServe()
	}()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			slog.Info("cloud metrics", "counts", server.Stats())
			for _, table := range []string{"tokens", "devices", "oauth_states"} {
				if _, err := db.ExecContext(ctx, "DELETE FROM "+table+" WHERE expires<$1", time.Now().Unix()); err != nil {
					slog.Error("expiry cleanup failed", "table", table)
				}
			}
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return httpServer.Shutdown(shutdown)
		}
	}
}
