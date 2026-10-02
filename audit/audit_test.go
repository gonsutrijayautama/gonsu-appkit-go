package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
)

type addrKey struct{}

// from memasang alamat asal ke ctx, seperti middleware produk.
func from(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, addrKey{}, netip.MustParseAddr(addr))
}

func setup(t *testing.T) (*audit.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	s, err := audit.New(pool, testdb.Hooks(), audit.Options{
		ClientAddr: func(ctx context.Context) netip.Addr {
			addr, _ := ctx.Value(addrKey{}).(netip.Addr)
			return addr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, pool
}

// user adalah pengguna biasa: boleh bertindak, tidak boleh membaca riwayat.
func user(org, id uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{Organization: org, User: id})
}

// admin memegang izin View.
func admin(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{
		Organization: org, User: uuid.New(), Permissions: []appkit.Permission{audit.View},
	})
}

func entry(action string) audit.Entry {
	return audit.Entry{Category: audit.CategoryActivity, Action: action, Summary: "Dokumen disetujui."}
}

func list(t *testing.T, s *audit.Service, org uuid.UUID, q audit.Query) audit.Page {
	t.Helper()
	page, err := s.List(admin(org), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return page
}

func TestRecord(t *testing.T) {
	s, _ := setup(t)
	org, actor := uuid.New(), uuid.New()

	err := s.Record(from(user(org, actor), "203.0.113.7"), audit.Entry{
		Category: audit.CategoryActivity, Action: "document.approved",
		Target:  audit.Target{Type: "document", ID: "SPK-0001"},
		Summary: "  SPK-0001 disetujui. ",
		Details: map[string]any{"fields": []string{"status"}},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	page := list(t, s, org, audit.Query{})
	if len(page.Data) != 1 || page.Next != nil {
		t.Fatalf("catatan = %+v", page)
	}
	e := page.Data[0]
	if e.Category != audit.CategoryActivity || e.Action != "document.approved" || e.Summary != "SPK-0001 disetujui." ||
		e.Target != (audit.Target{Type: "document", ID: "SPK-0001"}) || e.CreatedAt.IsZero() {
		t.Errorf("catatan = %+v", e)
	}
	if e.ActorID == nil || *e.ActorID != actor {
		t.Errorf("pelaku = %v, ingin %s", e.ActorID, actor)
	}
	if e.ClientAddr == nil || *e.ClientAddr != "203.0.113.7" {
		t.Errorf("alamat asal = %v", e.ClientAddr)
	}
	if raw, _ := json.Marshal(e.Details); string(raw) != `{"fields":["status"]}` {
		t.Errorf("rincian = %s", raw)
	}

	// Tanpa rincian, tanpa sasaran, dan tanpa alamat asal: bentuk JSON-nya
	// tetap lengkap.
	if err := s.Record(user(org, actor), entry("document.deleted")); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(list(t, s, org, audit.Query{}).Data[0])
	for _, field := range []string{`"details":{}`, `"client_addr":null`, `"target":{"type":"","id":""}`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("JSON tidak memuat %s: %s", field, raw)
		}
	}
}

// Alamat IPv4 yang dibungkus IPv6 dicatat sebagai IPv4, dan zona antarmuka
// tidak ikut tercatat.
func TestClientAddrIsNormalized(t *testing.T) {
	s, _ := setup(t)
	org := uuid.New()
	for addr, want := range map[string]string{
		"::ffff:198.51.100.4": "198.51.100.4",
		"fe80::1%eth0":        "fe80::1",
		"2001:db8::5":         "2001:db8::5",
	} {
		if err := s.Record(from(user(org, uuid.New()), addr), entry("document.approved")); err != nil {
			t.Fatalf("Record dari %s: %v", addr, err)
		}
		if got := list(t, s, org, audit.Query{Limit: 1}).Data[0].ClientAddr; got == nil || *got != want {
			t.Errorf("alamat %s tercatat %v, ingin %s", addr, got, want)
		}
	}
}

// Catatan yang salah bentuk adalah galat pemrogram, dan tidak tersimpan.
func TestRecordRejectsMalformedEntries(t *testing.T) {
	s, _ := setup(t)
	org := uuid.New()
	ctx := user(org, uuid.New())
	big := map[string]any{"isi": strings.Repeat("a", 17<<10)}

	for name, e := range map[string]audit.Entry{
		"kelompok kosong":          {Action: "document.approved", Summary: "A"},
		"kelompok tak dikenal":     {Category: "lain", Action: "document.approved", Summary: "A"},
		"tindakan kosong":          {Category: audit.CategoryActivity, Summary: "A"},
		"tindakan tanpa titik":     {Category: audit.CategoryActivity, Action: "approved", Summary: "A"},
		"tindakan huruf besar":     {Category: audit.CategoryActivity, Action: "Document.Approved", Summary: "A"},
		"tindakan terlalu panjang": {Category: audit.CategoryActivity, Action: "document." + strings.Repeat("a", 100), Summary: "A"},
		"ringkasan kosong":         {Category: audit.CategoryActivity, Action: "document.approved", Summary: "  "},
		"ringkasan panjang":        {Category: audit.CategoryActivity, Action: "document.approved", Summary: strings.Repeat("a", 301)},
		"sasaran panjang":          {Category: audit.CategoryActivity, Action: "document.approved", Summary: "A", Target: audit.Target{ID: strings.Repeat("a", 201)}},
		"rincian terlalu besar":    {Category: audit.CategoryActivity, Action: "document.approved", Summary: "A", Details: big},
		"rincian bukan JSON":       {Category: audit.CategoryActivity, Action: "document.approved", Summary: "A", Details: map[string]any{"f": func() {}}},
	} {
		t.Run(name, func(t *testing.T) {
			err := s.Record(ctx, e)
			if err == nil {
				t.Fatal("catatan salah bentuk diterima")
			}
			// Bukan galat isian pengguna: tidak boleh sampai ke layar sebagai
			// pesan validasi.
			if _, ok := errors.AsType[*appkit.Error](err); ok {
				t.Errorf("galat = %v, ingin galat biasa", err)
			}
		})
	}
	if page := list(t, s, org, audit.Query{}); len(page.Data) != 0 {
		t.Errorf("catatan salah bentuk tersimpan: %+v", page.Data)
	}
}

// Mencatat butuh organization dan pelaku; membaca butuh izin View. Galat dari
// pengait diteruskan apa adanya.
func TestSessionAndPermission(t *testing.T) {
	s, _ := setup(t)
	org := uuid.New()

	if err := s.Record(context.Background(), entry("document.approved")); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Record tanpa sesi = %v", err)
	}
	noUser := testdb.With(context.Background(), testdb.Session{Organization: org})
	if err := s.Record(noUser, entry("document.approved")); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Record tanpa pengguna = %v", err)
	}
	// Mencatat tidak butuh izin apa pun: pemanggilnya yang sudah memeriksa.
	if err := s.Record(user(org, uuid.New()), entry("document.approved")); err != nil {
		t.Errorf("Record pengguna biasa = %v", err)
	}

	if _, err := s.List(user(org, uuid.New()), audit.Query{}); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("List tanpa izin = %v", err)
	}
	if _, err := s.List(context.Background(), audit.Query{}); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("List tanpa sesi = %v", err)
	}
	if page := list(t, s, org, audit.Query{}); len(page.Data) != 1 {
		t.Errorf("percobaan yang ditolak tercatat: %+v", page.Data)
	}
}

func TestTenantIsolation(t *testing.T) {
	s, _ := setup(t)
	a, b := uuid.New(), uuid.New()
	for i := range 3 {
		if err := s.Record(user(a, uuid.New()), entry(fmt.Sprintf("document.step_%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Record(user(b, uuid.New()), entry("document.approved")); err != nil {
		t.Fatal(err)
	}

	if page := list(t, s, b, audit.Query{}); len(page.Data) != 1 || page.Data[0].Action != "document.approved" {
		t.Errorf("organization B melihat catatan A: %+v", page.Data)
	}
	if page := list(t, s, a, audit.Query{}); len(page.Data) != 3 {
		t.Errorf("catatan A = %+v", page.Data)
	}
	// Penanda halaman milik organization lain tidak membuka catatannya.
	first := list(t, s, a, audit.Query{Limit: 1})
	if first.Next == nil {
		t.Fatal("halaman pertama A tanpa penanda")
	}
	if page := list(t, s, b, audit.Query{Before: *first.Next}); len(page.Data) != 0 {
		t.Errorf("penanda halaman A dipakai organization B: %+v", page.Data)
	}
}

func TestListPagesAndFilters(t *testing.T) {
	s, _ := setup(t)
	org := uuid.New()
	ctx := user(org, uuid.New())
	for i := range 5 {
		e := entry(fmt.Sprintf("document.step_%d", i))
		if i%2 == 1 {
			e.Category = audit.CategorySettings
		}
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// Terbaru dulu, dua per halaman, tanpa catatan yang terlewat atau berulang.
	var got []string
	q := audit.Query{Limit: 2}
	for range 4 {
		page := list(t, s, org, q)
		for _, e := range page.Data {
			got = append(got, e.Action)
		}
		if page.Next == nil {
			break
		}
		q.Before = *page.Next
	}
	want := "document.step_4 document.step_3 document.step_2 document.step_1 document.step_0"
	if strings.Join(got, " ") != want {
		t.Errorf("urutan halaman = %v", got)
	}

	if page := list(t, s, org, audit.Query{Category: audit.CategorySettings}); len(page.Data) != 2 || page.Data[0].Action != "document.step_3" {
		t.Errorf("saringan kelompok = %+v", page.Data)
	}
	if page := list(t, s, org, audit.Query{Category: audit.CategorySession}); len(page.Data) != 0 || page.Data == nil {
		t.Errorf("kelompok tanpa catatan = %+v", page.Data)
	}
	_, err := s.List(admin(org), audit.Query{Category: "lain"})
	if e, ok := errors.AsType[*appkit.Error](err); !ok || e.Kind != appkit.KindValidation {
		t.Errorf("kelompok tak dikenal = %v, ingin galat validasi", err)
	}
	// Penanda yang tidak ada: halaman kosong, bukan galat.
	if page := list(t, s, org, audit.Query{Before: uuid.New()}); len(page.Data) != 0 {
		t.Errorf("penanda yang tidak ada = %+v", page.Data)
	}
	// Limit dibatasi 200.
	if page := list(t, s, org, audit.Query{Limit: 100000}); len(page.Data) != 5 {
		t.Errorf("limit besar = %d catatan", len(page.Data))
	}
}

// RecordTx: catatan tersimpan hanya bila perubahannya tersimpan.
func TestRecordTx(t *testing.T) {
	s, pool := setup(t)
	org := uuid.New()
	ctx := user(org, uuid.New())
	bg := context.Background()

	tx, err := pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTx(ctx, tx, entry("document.approved")); err != nil {
		t.Fatalf("RecordTx: %v", err)
	}
	if err := tx.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	if page := list(t, s, org, audit.Query{}); len(page.Data) != 0 {
		t.Errorf("catatan transaksi yang dibatalkan tersimpan: %+v", page.Data)
	}

	tx, err = pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTx(ctx, tx, entry("document.approved")); err != nil {
		t.Fatalf("RecordTx: %v", err)
	}
	if page := list(t, s, org, audit.Query{}); len(page.Data) != 0 {
		t.Errorf("catatan terlihat sebelum transaksinya selesai: %+v", page.Data)
	}
	if err := tx.Commit(bg); err != nil {
		t.Fatal(err)
	}
	if page := list(t, s, org, audit.Query{}); len(page.Data) != 1 {
		t.Errorf("catatan transaksi yang selesai = %+v", page.Data)
	}
}

// RecordFor: kejadian tanpa sesi — berhasil masuk, sesi yang diputus sistem.
func TestRecordFor(t *testing.T) {
	s, _ := setup(t)
	org, actor := uuid.New(), uuid.New()
	bg := context.Background()

	signedIn := audit.Entry{Category: audit.CategorySession, Action: "session.signed_in", Summary: "Masuk."}
	if err := s.RecordFor(from(bg, "203.0.113.9"), org, actor, signedIn); err != nil {
		t.Fatalf("RecordFor: %v", err)
	}
	revoked := audit.Entry{Category: audit.CategorySession, Action: "session.revoked", Summary: "Sesi diputus karena akses dicabut."}
	if err := s.RecordFor(bg, org, uuid.Nil, revoked); err != nil {
		t.Fatalf("RecordFor tanpa pelaku: %v", err)
	}
	if err := s.RecordFor(bg, uuid.Nil, actor, signedIn); err == nil {
		t.Error("RecordFor tanpa organization lolos")
	}

	page := list(t, s, org, audit.Query{Category: audit.CategorySession})
	if len(page.Data) != 2 {
		t.Fatalf("catatan = %+v", page.Data)
	}
	system, login := page.Data[0], page.Data[1]
	if system.ActorID != nil || system.Action != "session.revoked" {
		t.Errorf("catatan sistem = %+v", system)
	}
	if login.ActorID == nil || *login.ActorID != actor || login.ClientAddr == nil || *login.ClientAddr != "203.0.113.9" {
		t.Errorf("catatan masuk = %+v", login)
	}
	if raw, _ := json.Marshal(system); !strings.Contains(string(raw), `"actor_id":null`) {
		t.Errorf("JSON catatan sistem = %s", raw)
	}
}

// Catatan hanya dapat ditambah: database sendiri menolak UPDATE.
func TestEventsCannotBeEdited(t *testing.T) {
	s, pool := setup(t)
	org := uuid.New()
	if err := s.Record(user(org, uuid.New()), entry("document.approved")); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(context.Background(), `UPDATE appkit_audit_events SET summary = 'Disunting' WHERE organization_id = $1`, org)
	if err == nil || !strings.Contains(err.Error(), "hanya dapat ditambah") {
		t.Errorf("UPDATE = %v, ingin ditolak database", err)
	}
	if got := list(t, s, org, audit.Query{}).Data[0].Summary; got != "Dokumen disetujui." {
		t.Errorf("catatan tersunting: %q", got)
	}
}

func TestRoutes(t *testing.T) {
	s, _ := setup(t)
	org := uuid.New()
	for i := range 3 {
		if err := s.Record(user(org, uuid.New()), entry(fmt.Sprintf("document.step_%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", s.Routes()...)
	do := func(ctx context.Context, path string) (int, audit.Page, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		var page audit.Page
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		return rec.Code, page, rec.Body.String()
	}

	// Izin diperiksa sebelum saringan dibaca.
	if code, _, _ := do(user(org, uuid.New()), "/v1/audit-events?before=bukan-uuid"); code != http.StatusForbidden {
		t.Errorf("GET tanpa izin = %d, ingin 403", code)
	}
	if code, _, _ := do(context.Background(), "/v1/audit-events"); code != http.StatusUnauthorized {
		t.Errorf("GET tanpa sesi = %d, ingin 401", code)
	}

	code, page, raw := do(admin(org), "/v1/audit-events?limit=2")
	if code != http.StatusOK || len(page.Data) != 2 || page.Next == nil || page.Data[0].Action != "document.step_2" {
		t.Fatalf("GET = %d, %s", code, raw)
	}
	code, page, raw = do(admin(org), "/v1/audit-events?limit=2&before="+page.Next.String())
	if code != http.StatusOK || len(page.Data) != 1 || page.Next != nil || !strings.Contains(raw, `"next":null`) {
		t.Errorf("halaman kedua = %d, %s", code, raw)
	}
	if code, page, _ := do(admin(org), "/v1/audit-events?category=settings"); code != http.StatusOK || len(page.Data) != 0 {
		t.Errorf("saringan kelompok = %d, %+v", code, page.Data)
	}
	if code, _, raw := do(admin(org), "/v1/audit-events?category=lain"); code != http.StatusBadRequest || !strings.Contains(raw, `"category"`) {
		t.Errorf("kelompok tak dikenal = %d, %s", code, raw)
	}
	if code, _, raw := do(admin(org), "/v1/audit-events?before=bukan-uuid"); code != http.StatusBadRequest || !strings.Contains(raw, `"before"`) {
		t.Errorf("penanda salah bentuk = %d, %s", code, raw)
	}
	// Limit yang bukan angka jatuh ke bawaan.
	if code, page, _ := do(admin(uuid.New()), "/v1/audit-events?limit=semua"); code != http.StatusOK || len(page.Data) != 0 {
		t.Errorf("organization lain = %d, %+v", code, page.Data)
	}

	// Tidak ada jalur menulis, mengubah, atau menghapus dari klien.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/v1/audit-events", nil).WithContext(admin(org)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /audit-events = %d, ingin 405", method, rec.Code)
		}
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	pool := testdb.New(t)
	if _, err := audit.New(nil, testdb.Hooks(), audit.Options{}); err == nil {
		t.Error("New tanpa pool lolos")
	}
	if _, err := audit.New(pool, appkit.Hooks{}, audit.Options{}); err == nil {
		t.Error("New tanpa pengait lolos")
	}
	noUser := testdb.Hooks()
	noUser.User = nil
	if _, err := audit.New(pool, noUser, audit.Options{}); err == nil {
		t.Error("New tanpa Hooks.User lolos")
	}
}

// Nama pelaku dicatat saat tindakan dilakukan; mengganti nama sesudahnya
// tidak mengubah catatan lama.
func TestActorName(t *testing.T) {
	pool := testdb.New(t)
	names := map[uuid.UUID]string{}
	s, err := audit.New(pool, testdb.Hooks(), audit.Options{
		ActorName: func(_ context.Context, _, actor uuid.UUID) string { return names[actor] },
	})
	if err != nil {
		t.Fatal(err)
	}
	org, ani, budi := uuid.New(), uuid.New(), uuid.New()
	names[ani] = "  Ani Wijaya "
	names[budi] = strings.Repeat("b", 250)

	if err := s.Record(user(org, ani), entry("document.approved")); err != nil {
		t.Fatal(err)
	}
	names[ani] = "Ani Baru"
	if err := s.Record(user(org, budi), entry("document.deleted")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFor(context.Background(), org, uuid.Nil, entry("document.expired")); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(user(org, uuid.New()), entry("document.archived")); err != nil {
		t.Fatal(err)
	}

	page := list(t, s, org, audit.Query{})
	got := map[string]string{}
	for _, e := range page.Data {
		got[e.Action] = e.ActorName
	}
	if got["document.approved"] != "Ani Wijaya" {
		t.Errorf("nama saat itu = %q, ingin %q", got["document.approved"], "Ani Wijaya")
	}
	if n := got["document.deleted"]; len([]rune(n)) != 200 {
		t.Errorf("nama panjang tidak dipotong: %d karakter", len([]rune(n)))
	}
	// Tanpa pelaku, atau pelaku yang namanya tidak diketahui: kosong.
	if got["document.expired"] != "" || got["document.archived"] != "" {
		t.Errorf("nama kosong yang diharapkan = %q, %q", got["document.expired"], got["document.archived"])
	}
	if raw, _ := json.Marshal(page.Data[0]); !strings.Contains(string(raw), `"actor_name":`) {
		t.Errorf("JSON tanpa actor_name: %s", raw)
	}

	// Tanpa Options.ActorName: catatan tetap tersimpan, tanpa nama.
	plain, _ := setup(t)
	other := uuid.New()
	if err := plain.Record(user(other, ani), entry("document.approved")); err != nil {
		t.Fatal(err)
	}
	if e := list(t, plain, other, audit.Query{}).Data[0]; e.ActorName != "" {
		t.Errorf("nama tanpa ActorName = %q", e.ActorName)
	}
}
