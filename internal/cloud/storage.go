package cloud

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	_ "modernc.org/sqlite"
)

var validID = regexp.MustCompile(`^[a-f0-9]{48}$`)

type Blobs interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
	Delete(context.Context, string) error
}

type Files struct{ Root string }

func (f Files) path(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", errors.New("invalid blob id")
	}
	return filepath.Join(f.Root, id), nil
}
func (f Files) Put(ctx context.Context, id string, data []byte) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(f.Root, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(f.Root, ".upload-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
func (f Files) Get(ctx context.Context, id string) ([]byte, error) {
	path, err := f.path(id)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return boundedRead(file, MaxArchive+128)
}
func (f Files) Delete(ctx context.Context, id string) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type S3 struct {
	Client *minio.Client
	Bucket string
}

func NewS3(endpoint, bucket, key, secret string, secure bool) (*S3, error) {
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Secure: secure})
	if err != nil {
		return nil, err
	}
	return &S3{client, bucket}, nil
}
func (s *S3) Put(ctx context.Context, id string, data []byte) error {
	if !validID.MatchString(id) {
		return errors.New("invalid blob id")
	}
	_, err := s.Client.PutObject(ctx, s.Bucket, id, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}
func (s *S3) Get(ctx context.Context, id string) ([]byte, error) {
	if !validID.MatchString(id) {
		return nil, errors.New("invalid blob id")
	}
	o, err := s.Client.GetObject(ctx, s.Bucket, id, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer o.Close()
	return boundedRead(o, MaxArchive+128)
}
func (s *S3) Delete(ctx context.Context, id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid blob id")
	}
	return s.Client.RemoveObject(ctx, s.Bucket, id, minio.RemoveObjectOptions{})
}

// EncryptedBlobs authenticates each object and binds the ciphertext to its ID.
type EncryptedBlobs struct {
	Base Blobs
	aead cipher.AEAD
}

func EncryptBlobs(base Blobs, key []byte) (*EncryptedBlobs, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &EncryptedBlobs{base, aead}, nil
}
func (e *EncryptedBlobs) Put(ctx context.Context, id string, data []byte) error {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return e.Base.Put(ctx, id, e.aead.Seal(nonce, nonce, data, []byte(id)))
}
func (e *EncryptedBlobs) Get(ctx context.Context, id string) ([]byte, error) {
	data, err := e.Base.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	n := e.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("invalid encrypted blob")
	}
	return e.aead.Open(nil, data[:n], data[n:], []byte(id))
}
func (e *EncryptedBlobs) Delete(ctx context.Context, id string) error { return e.Base.Delete(ctx, id) }

func boundedRead(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err == nil && int64(len(data)) > max {
		return nil, errors.New("size limit exceeded")
	}
	return data, err
}

// OpenDB accepts pgx for production and sqlite for local development/tests.
func OpenDB(ctx context.Context, driver, dsn string) (*sql.DB, error) {
	if driver != "pgx" && driver != "sqlite" {
		return nil, errors.New("database driver must be pgx or sqlite")
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(10)
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate is also exposed as a separate deployment command. Version 1 is
// additive and serialized by the Postgres transaction advisory lock.
func Migrate(ctx context.Context, db *sql.DB, driver string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if driver == "pgx" {
		if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(734819203)"); err != nil {
			return err
		}
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, login TEXT NOT NULL UNIQUE, bytes_used BIGINT NOT NULL DEFAULT 0, publications INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS teams (id TEXT PRIMARY KEY, name TEXT NOT NULL, created BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS team_members (team_id TEXT NOT NULL, user_id TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('owner','writer','reader')), PRIMARY KEY(team_id,user_id))`,
		`CREATE TABLE IF NOT EXISTS tokens (hash TEXT PRIMARY KEY, user_id TEXT NOT NULL, expires BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS devices (id TEXT PRIMARY KEY, secret_hash TEXT NOT NULL, user_id TEXT NOT NULL DEFAULT '', expires BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS oauth_states (hash TEXT PRIMARY KEY, verifier TEXT NOT NULL, device TEXT NOT NULL, next_path TEXT NOT NULL DEFAULT '', expires BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS publications (id TEXT PRIMARY KEY, owner TEXT NOT NULL, visibility TEXT NOT NULL CHECK(visibility IN ('private','unlisted','public')), expires BIGINT NOT NULL, created BIGINT NOT NULL, size BIGINT NOT NULL, request_key TEXT NOT NULL, digest TEXT NOT NULL, parent TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0, UNIQUE(owner,request_key))`,
		`CREATE TABLE IF NOT EXISTS grants (publication TEXT NOT NULL, user_id TEXT NOT NULL, PRIMARY KEY(publication,user_id))`,
		`CREATE TABLE IF NOT EXISTS audit_events (id TEXT PRIMARY KEY, actor TEXT NOT NULL, action TEXT NOT NULL, publication TEXT NOT NULL, created BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS waitlist (email TEXT PRIMARY KEY, created BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS publications_owner ON publications(owner,created)`,
		`CREATE INDEX IF NOT EXISTS tokens_expiry ON tokens(expires)`,
		`INSERT INTO schema_migrations(version) VALUES(1) ON CONFLICT(version) DO NOTHING`,
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	return tx.Commit()
}
