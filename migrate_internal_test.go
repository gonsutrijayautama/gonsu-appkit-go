package appkit

import (
	"context"
	"crypto/rand"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// emptyDatabase membuat database kosong sekali pakai. Sama dengan
// internal/testdb.Empty, yang tidak dapat dipakai di sini: test di dalam
// package appkit tidak boleh meng-import package yang meng-import appkit.
func emptyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL kosong; jalankan lewat `make test`")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("koneksi admin: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "appkit_test_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("DROP DATABASE %s: %v", name, err)
		}
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("koneksi: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Migrasi 00011 membuang gambar bagian "tentang" beserta berkasnya, tanpa
// menyentuh gambar lain, isi pengaturan, atau berkas organization lain.
func TestWebsiteIdentityMigrationRemovesAboutImages(t *testing.T) {
	pool := emptyDatabase(t)
	ctx := context.Background()

	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithTableName(versionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	// Keadaan sebelum migrasi itu.
	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatalf("migrasi sampai 00010: %v", err)
	}

	org, other := uuid.New(), uuid.New()
	about, seo, logo, foreign := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	sha := strings.Repeat("a", 64)
	for _, f := range []struct{ org, id uuid.UUID }{{org, about}, {org, seo}, {org, logo}, {other, foreign}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO appkit_media (id, organization_id, content_type, size_bytes, sha256)
			VALUES ($1, $2, 'image/png', 10, $3)`, f.id, f.org, sha); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO appkit_media_blobs (key, content) VALUES ($1, 'isi')`,
			f.org.String()+"/"+f.id.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO appkit_websites (organization_id, settings, about_media_id, seo_media_id, version)
		VALUES ($1, '{"schema":1,"mode":"site","tagline":"Rapi","about_text":"Lama","services":[{"title":"Jahit"}]}', $2, $3, 4)`,
		org, about, seo); err != nil {
		t.Fatal(err)
	}
	// Organization tanpa gambar "tentang" tidak tersentuh.
	if _, err := pool.Exec(ctx, `INSERT INTO appkit_websites (organization_id, seo_media_id) VALUES ($1, $2)`, other, foreign); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	exists := func(query string, args ...any) bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (`+query+`)`, args...).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if exists(`SELECT 1 FROM information_schema.columns WHERE table_name = 'appkit_websites' AND column_name = 'about_media_id'`) {
		t.Error("kolom about_media_id masih ada")
	}
	if exists(`SELECT 1 FROM appkit_media WHERE id = $1`, about) || exists(`SELECT 1 FROM appkit_media_blobs WHERE key = $1`, org.String()+"/"+about.String()) {
		t.Error("gambar tentang atau isinya masih ada")
	}
	for name, f := range map[string]struct{ org, id uuid.UUID }{"pratinjau": {org, seo}, "logo": {org, logo}, "organization lain": {other, foreign}} {
		if !exists(`SELECT 1 FROM appkit_media WHERE id = $1`, f.id) || !exists(`SELECT 1 FROM appkit_media_blobs WHERE key = $1`, f.org.String()+"/"+f.id.String()) {
			t.Errorf("berkas %s ikut terhapus", name)
		}
	}
	// Isi pengaturan dan version-nya tidak disentuh.
	var (
		tagline string
		version int
		linked  *uuid.UUID
	)
	if err := pool.QueryRow(ctx, `SELECT settings->>'tagline', version, seo_media_id FROM appkit_websites WHERE organization_id = $1`, org).
		Scan(&tagline, &version, &linked); err != nil {
		t.Fatal(err)
	}
	if tagline != "Rapi" || version != 4 || linked == nil || *linked != seo {
		t.Errorf("pengaturan sesudah migrasi = %q, version %d, gambar %v", tagline, version, linked)
	}
}
