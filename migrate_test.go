package appkit_test

import (
	"context"
	"testing"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
)

// Migrasi library memakai tabel versinya sendiri, sehingga penomorannya
// tidak pernah bertabrakan dengan migrasi produk — dan menjalankannya dua
// kali tidak mengubah apa pun.
func TestMigrateUsesItsOwnVersionTable(t *testing.T) {
	pool := testdb.Empty(t)
	ctx := context.Background()

	for range 2 {
		if err := appkit.Migrate(ctx, pool, nil); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}

	exists := func(name string) bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for _, table := range []string{"appkit_schema_migrations", "appkit_media", "appkit_media_blobs", "appkit_business_profiles"} {
		if !exists(table) {
			t.Errorf("tabel %s tidak dibuat", table)
		}
	}
	if exists("goose_db_version") {
		t.Error("migrasi library menulis ke goose_db_version, tabel versi milik produk")
	}
}

// Setiap tabel library berawalan appkit_: nama tanpa awalan dapat bertabrakan
// dengan tabel produk.
func TestEveryTableIsPrefixed(t *testing.T) {
	pool := testdb.New(t)
	rows, err := pool.Query(context.Background(), `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename NOT LIKE 'appkit\_%' AND tablename <> 'goose_lock'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("tabel %s tidak berawalan appkit_", name)
	}
}

func TestHooksValidate(t *testing.T) {
	full := testdb.Hooks()
	if err := full.Validate(); err != nil {
		t.Fatalf("pengait lengkap ditolak: %v", err)
	}
	for name, h := range map[string]appkit.Hooks{
		"Organization": {Authorize: full.Authorize, WriteError: full.WriteError},
		"Authorize":    {Organization: full.Organization, WriteError: full.WriteError},
		"WriteError":   {Organization: full.Organization, Authorize: full.Authorize},
	} {
		if err := h.Validate(); err == nil {
			t.Errorf("pengait tanpa %s lolos", name)
		}
	}
}
