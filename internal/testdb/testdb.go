// Package testdb menyiapkan PostgreSQL sekali pakai dan pengait palsu untuk
// test. Hanya dipakai berkas _test.go.
//
// Test di-skip bila TEST_DATABASE_URL kosong; `make test` mengisinya dengan
// database compose.dev.yaml, dan CI dengan service container.
package testdb

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// New membuat database kosong, menjalankan migrasi library, dan
// mengembalikan pool-nya. Database dihapus saat test selesai.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := Empty(t)
	if err := appkit.Migrate(context.Background(), pool, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

// Empty membuat database kosong tanpa migrasi.
func Empty(t *testing.T) *pgxpool.Pool {
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
		t.Fatalf("TEST_DATABASE_URL harus berbentuk URL: %v", err)
	}
	u.Path = "/" + name

	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(connectCtx, u.String())
	if err != nil {
		t.Fatalf("koneksi: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Session adalah sesi palsu sebuah request: organization dan izin yang
// dipegang penggunanya.
type Session struct {
	Organization uuid.UUID
	Permissions  []appkit.Permission
}

type sessionKey struct{}

// With memasang sesi palsu ke ctx.
func With(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// ErrDenied adalah galat milik "produk" saat izin ditolak. Modul harus
// meneruskannya apa adanya.
var ErrDenied = errors.New("produk: izin ditolak")

// ErrNoSession adalah galat milik "produk" saat request tanpa sesi.
var ErrNoSession = errors.New("produk: tanpa sesi")

// Hooks mengembalikan pengait yang membaca Session dari context dan menulis
// galat seperti produk: *appkit.Error menurut jenisnya, ErrDenied 403,
// ErrNoSession 401, sisanya 500.
func Hooks() appkit.Hooks {
	return appkit.Hooks{
		Organization: func(ctx context.Context) (uuid.UUID, error) {
			s, ok := ctx.Value(sessionKey{}).(Session)
			if !ok || s.Organization == uuid.Nil {
				return uuid.Nil, ErrNoSession
			}
			return s.Organization, nil
		},
		Authorize: func(ctx context.Context, perm appkit.Permission) error {
			s, ok := ctx.Value(sessionKey{}).(Session)
			if !ok {
				return ErrNoSession
			}
			if !slices.Contains(s.Permissions, perm) {
				return ErrDenied
			}
			return nil
		},
		WriteError: func(w http.ResponseWriter, _ *http.Request, err error) {
			status, body := http.StatusInternalServerError, map[string]any{"message": "galat tak terduga"}
			if e, ok := errors.AsType[*appkit.Error](err); ok {
				body = map[string]any{"kind": e.Kind, "message": e.Message, "fields": e.Fields}
				switch e.Kind {
				case appkit.KindValidation:
					status = http.StatusBadRequest
				case appkit.KindNotFound:
					status = http.StatusNotFound
				}
			} else if errors.Is(err, ErrDenied) {
				status, body = http.StatusForbidden, map[string]any{"message": "ditolak"}
			} else if errors.Is(err, ErrNoSession) {
				status, body = http.StatusUnauthorized, map[string]any{"message": "tanpa sesi"}
			}
			httpjson.Write(w, status, body)
		},
	}
}
