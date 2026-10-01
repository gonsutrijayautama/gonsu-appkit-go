package appkit

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// versionTable mencatat migrasi library, terpisah dari `goose_db_version`
// milik produk: penomoran migrasi library tidak pernah bertabrakan dengan
// penomoran migrasi produk.
const versionTable = "appkit_schema_migrations"

// lockID membedakan lock migrasi library dari lock migrasi produk di tabel
// lock yang sama (`goose_lock`). Nilainya tetap; mengubahnya membuat dua
// versi library dapat bermigrasi bersamaan.
const lockID int64 = 7102250931

// Waktu tunggu lock sama dengan migrasi produk di template gonsu-cli: replica
// yang menunggu harus menyerah sebelum liveness probe chart GONSU membunuh
// pod, supaya kegagalannya menjadi pesan yang menyebut sebabnya.
const (
	lockRetryInterval  = time.Second
	lockRetryThreshold = 12
)

// Migrate menerapkan migrasi library yang belum diterapkan.
//
// Panggil saat start SEBELUM migrasi produk: tabel library tidak pernah
// merujuk tabel produk, sedangkan tabel produk boleh merujuk tabel library
// (mis. foreign key ke appkit_media).
//
// Lock-nya berbasis tabel, bukan advisory lock tingkat sesi, dengan alasan
// yang sama seperti migrasi produk: database pelanggan berada di belakang
// PgBouncer `pool_mode=transaction`, dan advisory lock tingkat sesi di sana
// tidak menjaga apa pun.
//
// Migrasi yang gagal mengembalikan galat, dan pemanggil wajib menghentikan
// startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("appkit: membaca migrasi: %w", err)
	}
	locker, err := lock.NewPostgresTableLocker(
		lock.WithTableLockID(lockID),
		lock.WithTableLockTimeout(lockRetryInterval, lockRetryThreshold),
		lock.WithTableLeaseDuration(30*time.Second),
		lock.WithTableHeartbeatInterval(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("appkit: menyiapkan lock migrasi: %w", err)
	}

	// Menutup db ini tidak menutup pool.
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithTableName(versionTable),
		goose.WithLocker(locker),
		// Migrasi Go yang didaftarkan produk secara global bukan milik library.
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return fmt.Errorf("appkit: menyiapkan migrasi: %w", err)
	}

	results, err := provider.Up(ctx)
	if partial, ok := errors.AsType[*goose.PartialError](err); ok {
		return fmt.Errorf("appkit: migrasi %s gagal: %w", partial.Failed.Source.Path, partial.Err)
	}
	if err != nil {
		return fmt.Errorf("appkit: migrasi gagal: %w", err)
	}
	for _, r := range results {
		logger.InfoContext(ctx, "migrasi appkit diterapkan",
			slog.Int64("version", r.Source.Version),
			slog.String("file", r.Source.Path),
			slog.Float64("duration_ms", float64(r.Duration.Microseconds())/1000))
	}
	return nil
}
