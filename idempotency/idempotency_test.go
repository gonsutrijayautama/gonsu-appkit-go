package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/idempotency"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
)

// endpoint adalah operasi "produk" di test ini.
const endpoint = "POST /v1/invoices"

type fixture struct {
	pool *pgxpool.Pool
	idem *idempotency.Service
}

func setup(t *testing.T) *fixture {
	t.Helper()
	pool := testdb.New(t)
	s, err := idempotency.New(pool, idempotency.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{pool: pool, idem: s}
}

// scope adalah key milik org untuk request berisi body.
func scope(org uuid.UUID, key, body string) idempotency.Scope {
	return idempotency.Scope{
		OrganizationID: org, Endpoint: endpoint, Key: key,
		Fingerprint: idempotency.Fingerprint([]byte(body)),
	}
}

func kind(err error) appkit.Kind {
	if e, ok := errors.AsType[*appkit.Error](err); ok {
		return e.Kind
	}
	return ""
}

// begin membuka transaksi "dokumen". Yang belum diakhiri test dibatalkan saat
// test selesai.
func (f *fixture) begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func (f *fixture) claim(t *testing.T, tx pgx.Tx, sc idempotency.Scope) bool {
	t.Helper()
	claimed, err := f.idem.Claim(context.Background(), tx, sc)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return claimed
}

func commit(t *testing.T, tx pgx.Tx) {
	t.Helper()
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// store menjalankan alur penuh sebuah mutasi: Claim, Complete, commit.
func (f *fixture) store(t *testing.T, sc idempotency.Scope, resp idempotency.Response) {
	t.Helper()
	tx := f.begin(t)
	if !f.claim(t, tx, sc) {
		t.Fatalf("Claim %s = false, ingin true", sc.Key)
	}
	if err := f.idem.Complete(context.Background(), tx, sc, resp); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	commit(t, tx)
}

func (f *fixture) replay(t *testing.T, sc idempotency.Scope) *idempotency.Response {
	t.Helper()
	resp, err := f.idem.Replay(context.Background(), sc)
	if err != nil {
		t.Fatalf("Replay %s: %v", sc.Key, err)
	}
	return resp
}

// expire memundurkan kedaluwarsa key sc ke masa lalu.
func (f *fixture) expire(t *testing.T, sc idempotency.Scope) {
	t.Helper()
	tag, err := f.pool.Exec(context.Background(), `
		UPDATE appkit_idempotency_keys SET expires_at = now() - interval '1 second'
		WHERE organization_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
		sc.OrganizationID, sc.Endpoint, sc.Key)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("memundurkan kedaluwarsa %s: %d baris, %v", sc.Key, tag.RowsAffected(), err)
	}
}

// rows adalah jumlah baris key yang tersimpan, hidup maupun kedaluwarsa.
func (f *fixture) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM appkit_idempotency_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Alur penuh: key yang belum dipakai tidak diputar ulang, klaim yang belum
// di-commit belum terlihat, dan sesudah commit response-nya kembali byte per
// byte.
func TestReplayReturnsTheStoredResponse(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	sc := scope(org, "kunci-pertama", `{"total":1}`)
	// Spasi, urutan field, dan baris baru yang akan dirapikan jsonb, ditambah
	// byte yang bukan UTF-8: semuanya harus kembali apa adanya.
	body := []byte("{\"b\": 1,  \"a\":2, \"a\":3}\n\x00\xff")

	if resp := f.replay(t, sc); resp != nil {
		t.Fatalf("Replay key yang belum dipakai = %+v, ingin nil", resp)
	}

	tx := f.begin(t)
	if !f.claim(t, tx, sc) {
		t.Fatal("Claim key baru = false")
	}
	if err := f.idem.Complete(context.Background(), tx, sc, idempotency.Response{Status: http.StatusCreated, Body: body}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp := f.replay(t, sc); resp != nil {
		t.Errorf("Replay sebelum commit = %+v, ingin nil", resp)
	}
	commit(t, tx)

	for range 2 {
		resp := f.replay(t, sc)
		if resp == nil || resp.Status != http.StatusCreated || !bytes.Equal(resp.Body, body) || !resp.Replayed {
			t.Fatalf("Replay = %+v, ingin status 201, body identik, Replayed", resp)
		}
	}

	// Response tanpa body juga diputar ulang.
	empty := scope(org, "kunci-tanpa-body", "")
	f.store(t, empty, idempotency.Response{Status: http.StatusNoContent})
	if resp := f.replay(t, empty); resp == nil || resp.Status != http.StatusNoContent || len(resp.Body) != 0 || !resp.Replayed {
		t.Errorf("Replay response tanpa body = %+v", resp)
	}
}

// Key sama dengan isi berbeda adalah kesalahan client, bukan pengulangan.
func TestSameKeyWithDifferentBodyConflicts(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	sc := scope(org, "kunci-pertama", `{"total":1}`)
	f.store(t, sc, idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":1}`)})

	resp, err := f.idem.Replay(context.Background(), scope(org, "kunci-pertama", `{"total":2}`))
	if resp != nil || kind(err) != appkit.KindIdempotencyConflict || !strings.Contains(err.Error(), "isi berbeda") {
		t.Fatalf("Replay isi berbeda = %+v, %v; ingin galat idempotency_conflict", resp, err)
	}

	// "Produk" di test ini menjawabnya 409.
	rec := httptest.NewRecorder()
	testdb.Hooks().WriteError(rec, httptest.NewRequest(http.MethodPost, "/v1/invoices", nil), err)
	var out struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusConflict || out.Kind != "idempotency_conflict" {
		t.Errorf("WriteError = %d %s (%v)", rec.Code, rec.Body, err)
	}

	// Response yang tersimpan tidak terganggu.
	if resp := f.replay(t, sc); resp == nil || string(resp.Body) != `{"id":1}` {
		t.Errorf("Replay isi yang sama = %+v", resp)
	}
}

// Key yang diklaim tanpa response tersimpan dijawab "masih diproses", bukan
// diputar ulang sebagai response kosong.
func TestClaimedWithoutResponseConflicts(t *testing.T) {
	f := setup(t)
	sc := scope(uuid.New(), "kunci-pertama", `{"total":1}`)

	tx := f.begin(t)
	if !f.claim(t, tx, sc) {
		t.Fatal("Claim key baru = false")
	}
	commit(t, tx)

	resp, err := f.idem.Replay(context.Background(), sc)
	if resp != nil || kind(err) != appkit.KindIdempotencyConflict || !strings.Contains(err.Error(), "masih diproses") {
		t.Errorf("Replay key tanpa response = %+v, %v; ingin galat idempotency_conflict", resp, err)
	}
	// Isi berbeda tetap dijawab "isi berbeda".
	_, err = f.idem.Replay(context.Background(), scope(sc.OrganizationID, sc.Key, `{"total":2}`))
	if kind(err) != appkit.KindIdempotencyConflict || !strings.Contains(err.Error(), "isi berbeda") {
		t.Errorf("Replay isi berbeda = %v", err)
	}
}

// Key yang masih hidup tidak dapat diklaim lagi, dengan isi apa pun.
func TestLiveKeyIsNotClaimedTwice(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	sc := scope(org, "kunci-pertama", `{"total":1}`)
	first := idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":1}`)}
	f.store(t, sc, first)

	for _, again := range []idempotency.Scope{sc, scope(org, "kunci-pertama", `{"total":2}`)} {
		tx := f.begin(t)
		if f.claim(t, tx, again) {
			t.Errorf("Claim kedua (sidik jari %s) = true, ingin false", again.Fingerprint[:8])
		}
		// Yang klaimnya ditolak tidak dapat menimpa response yang tersimpan.
		err := f.idem.Complete(context.Background(), tx, again, idempotency.Response{Status: http.StatusOK, Body: []byte(`{"id":2}`)})
		if err == nil || kind(err) != "" {
			t.Errorf("Complete tanpa klaim = %v, ingin galat pemrogram", err)
		}
		commit(t, tx)
	}

	if resp := f.replay(t, sc); resp == nil || resp.Status != first.Status || !bytes.Equal(resp.Body, first.Body) {
		t.Errorf("response tersimpan berubah: %+v", resp)
	}
	if n := f.rows(t); n != 1 {
		t.Errorf("baris key = %d, ingin 1", n)
	}
}

// Transaksi dokumen yang dibatalkan melepas klaimnya: tidak ada yang
// tersimpan, dan key yang sama dapat dipakai lagi.
func TestRollbackReleasesTheClaim(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sc := scope(uuid.New(), "kunci-pertama", `{"total":1}`)

	tx := f.begin(t)
	if !f.claim(t, tx, sc) {
		t.Fatal("Claim key baru = false")
	}
	if err := f.idem.Complete(ctx, tx, sc, idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":1}`)}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if n := f.rows(t); n != 0 {
		t.Errorf("baris key sesudah rollback = %d, ingin 0", n)
	}
	if resp := f.replay(t, sc); resp != nil {
		t.Errorf("Replay sesudah rollback = %+v, ingin nil", resp)
	}

	f.store(t, sc, idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":2}`)})
	if resp := f.replay(t, sc); resp == nil || string(resp.Body) != `{"id":2}` {
		t.Errorf("Replay sesudah percobaan kedua = %+v", resp)
	}
}

// Key yang kedaluwarsa diperlakukan seperti belum pernah dipakai: tidak
// diputar ulang, tidak bentrok, dan dapat diklaim ulang.
func TestExpiredKeyIsClaimedAgain(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	org := uuid.New()
	old := scope(org, "kunci-pertama", `{"total":1}`)
	f.store(t, old, idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":1}`)})
	f.expire(t, old)

	next := scope(org, "kunci-pertama", `{"total":2}`)
	for _, sc := range []idempotency.Scope{old, next} {
		if resp := f.replay(t, sc); resp != nil {
			t.Errorf("Replay key kedaluwarsa = %+v, ingin nil", resp)
		}
	}

	tx := f.begin(t)
	if !f.claim(t, tx, next) {
		t.Fatal("Claim key kedaluwarsa = false, ingin true")
	}
	// Response lama dibuang saat diklaim ulang, dan umur key dihitung dari awal.
	var cleared, fresh bool
	if err := tx.QueryRow(ctx, `
		SELECT response_status IS NULL AND response_body IS NULL, created_at = now() AND expires_at > now()
		FROM appkit_idempotency_keys WHERE organization_id = $1`, org).Scan(&cleared, &fresh); err != nil {
		t.Fatal(err)
	}
	if !cleared || !fresh {
		t.Errorf("sesudah klaim ulang: response dibuang = %v, umur baru = %v", cleared, fresh)
	}
	if err := f.idem.Complete(ctx, tx, next, idempotency.Response{Status: http.StatusOK, Body: []byte(`{"id":2}`)}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	commit(t, tx)

	if resp := f.replay(t, next); resp == nil || resp.Status != http.StatusOK || string(resp.Body) != `{"id":2}` {
		t.Errorf("Replay sesudah klaim ulang = %+v", resp)
	}
	// Yang hidup sekarang permintaan kedua; isi permintaan lama menjadi "isi
	// berbeda".
	if _, err := f.idem.Replay(ctx, old); kind(err) != appkit.KindIdempotencyConflict {
		t.Errorf("Replay isi lama = %v, ingin galat idempotency_conflict", err)
	}
	if n := f.rows(t); n != 1 {
		t.Errorf("baris key = %d, ingin 1", n)
	}
}

// Key berlaku per organization. Key milik organization lain dijawab seperti
// belum pernah dipakai — bukan "isi berbeda", yang membocorkan keberadaannya.
func TestOrganizationsDoNotSeeEachOther(t *testing.T) {
	f := setup(t)
	a, b := uuid.New(), uuid.New()
	mine := scope(a, "kunci-bersama", `{"total":1}`)
	f.store(t, mine, idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"org":"a"}`)})

	for _, body := range []string{`{"total":1}`, `{"total":2}`} {
		resp, err := f.idem.Replay(context.Background(), scope(b, "kunci-bersama", body))
		if resp != nil || err != nil {
			t.Errorf("Replay dari organization lain = %+v, %v; ingin nil", resp, err)
		}
	}

	theirs := scope(b, "kunci-bersama", `{"total":2}`)
	f.store(t, theirs, idempotency.Response{Status: http.StatusOK, Body: []byte(`{"org":"b"}`)})

	if resp := f.replay(t, mine); resp == nil || string(resp.Body) != `{"org":"a"}` {
		t.Errorf("Replay organization a = %+v", resp)
	}
	if resp := f.replay(t, theirs); resp == nil || string(resp.Body) != `{"org":"b"}` {
		t.Errorf("Replay organization b = %+v", resp)
	}
}

// Key yang sama di dua endpoint adalah dua key.
func TestEndpointsAreIndependent(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	invoices := scope(org, "kunci-bersama", `{"total":1}`)
	payments := invoices
	payments.Endpoint = "POST /v1/payments"
	payments.Fingerprint = idempotency.Fingerprint([]byte(`{"amount":5}`))

	f.store(t, invoices, idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"invoice":1}`)})
	if resp, err := f.idem.Replay(context.Background(), payments); resp != nil || err != nil {
		t.Fatalf("Replay di endpoint lain = %+v, %v; ingin nil", resp, err)
	}
	f.store(t, payments, idempotency.Response{Status: http.StatusAccepted, Body: []byte(`{"payment":1}`)})

	if resp := f.replay(t, invoices); resp == nil || resp.Status != http.StatusCreated || string(resp.Body) != `{"invoice":1}` {
		t.Errorf("Replay invoices = %+v", resp)
	}
	if resp := f.replay(t, payments); resp == nil || resp.Status != http.StatusAccepted || string(resp.Body) != `{"payment":1}` {
		t.Errorf("Replay payments = %+v", resp)
	}
}

// DeleteExpired menghapus baris kedaluwarsa di semua organization, dan hanya
// itu.
func TestDeleteExpired(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	resp := idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":1}`)}

	if n, err := f.idem.DeleteExpired(ctx); n != 0 || err != nil {
		t.Fatalf("DeleteExpired di tabel kosong = %d, %v", n, err)
	}

	live := scope(a, "kunci-hidup", "1")
	f.store(t, live, resp)
	for _, sc := range []idempotency.Scope{scope(a, "kunci-lama-a", "2"), scope(b, "kunci-lama-b", "3")} {
		f.store(t, sc, resp)
		f.expire(t, sc)
	}
	// Klaim tanpa response yang kedaluwarsa ikut dihapus.
	stuck := scope(b, "kunci-macet", "4")
	tx := f.begin(t)
	if !f.claim(t, tx, stuck) {
		t.Fatal("Claim key baru = false")
	}
	commit(t, tx)
	f.expire(t, stuck)

	if n, err := f.idem.DeleteExpired(ctx); n != 3 || err != nil {
		t.Fatalf("DeleteExpired = %d, %v; ingin 3", n, err)
	}
	if n := f.rows(t); n != 1 {
		t.Errorf("baris tersisa = %d, ingin 1", n)
	}
	if got := f.replay(t, live); got == nil || !bytes.Equal(got.Body, resp.Body) {
		t.Errorf("key yang masih hidup ikut terhapus: %+v", got)
	}
	if n, err := f.idem.DeleteExpired(ctx); n != 0 || err != nil {
		t.Errorf("DeleteExpired kedua = %d, %v; ingin 0", n, err)
	}
}

func TestKey(t *testing.T) {
	request := func(values ...string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/invoices", nil)
		for _, v := range values {
			r.Header.Add(idempotency.Header, v)
		}
		return r
	}

	if idempotency.Header != "Idempotency-Key" {
		t.Errorf("Header = %q", idempotency.Header)
	}

	for _, key := range []string{
		"abcdefgh",
		"A1._:-xyz",
		uuid.NewString(),
		"order:2026-10-03:0001",
		strings.Repeat("k", 200),
	} {
		if got, err := idempotency.Key(request(key)); err != nil || got != key {
			t.Errorf("Key(%q) = %q, %v", key, got, err)
		}
	}

	for name, tc := range map[string]struct {
		values []string
		want   string
	}{
		"tanpa header":       {nil, "wajib diisi"},
		"header kosong":      {[]string{""}, "wajib diisi"},
		"tujuh karakter":     {[]string{"abcdefg"}, "tidak valid"},
		"201 karakter":       {[]string{strings.Repeat("k", 201)}, "tidak valid"},
		"spasi":              {[]string{"abcd efgh"}, "tidak valid"},
		"spasi di tepi":      {[]string{" abcdefgh"}, "tidak valid"},
		"garis miring":       {[]string{"abcd/efgh"}, "tidak valid"},
		"bukan ASCII":        {[]string{"kunci-é-panjang"}, "tidak valid"},
		"baris baru di ekor": {[]string{"abcdefgh\n"}, "tidak valid"},
		"tanda kutip":        {[]string{`abcdefgh"`}, "tidak valid"},
	} {
		got, err := idempotency.Key(request(tc.values...))
		if got != "" || kind(err) != appkit.KindValidation || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Key = %q, %v; ingin galat validasi %q", name, got, err, tc.want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	// SHA-256 atas body kosong, dalam heksadesimal.
	if got := idempotency.Fingerprint(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("Fingerprint(nil) = %s", got)
	}
	a, b := idempotency.Fingerprint([]byte(`{"total":1}`)), idempotency.Fingerprint([]byte(`{"total": 1}`))
	if a == b || len(a) != 64 || a != idempotency.Fingerprint([]byte(`{"total":1}`)) {
		t.Errorf("Fingerprint = %s, %s", a, b)
	}
}

// Scope yang tidak lengkap adalah galat pemrogram: ditolak sebelum menyentuh
// database, dan bukan galat yang ditampilkan ke pengguna.
func TestScopeIsValidated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	valid := scope(uuid.New(), "kunci-pertama", `{"total":1}`)
	resp := idempotency.Response{Status: http.StatusCreated}

	for name, change := range map[string]func(*idempotency.Scope){
		"tanpa organization":       func(sc *idempotency.Scope) { sc.OrganizationID = uuid.Nil },
		"tanpa endpoint":           func(sc *idempotency.Scope) { sc.Endpoint = "" },
		"tanpa key":                func(sc *idempotency.Scope) { sc.Key = "" },
		"tanpa fingerprint":        func(sc *idempotency.Scope) { sc.Fingerprint = "" },
		"endpoint terlalu panjang": func(sc *idempotency.Scope) { sc.Endpoint = strings.Repeat("e", 201) },
		"key terlalu panjang":      func(sc *idempotency.Scope) { sc.Key = strings.Repeat("k", 201) },
		"fingerprint terlalu panjang": func(sc *idempotency.Scope) {
			sc.Fingerprint = strings.Repeat("f", 201)
		},
	} {
		sc := valid
		change(&sc)
		programmer := func(call string, err error) {
			t.Helper()
			if err == nil || kind(err) != "" {
				t.Errorf("%s: %s = %v, ingin galat pemrogram", name, call, err)
			}
		}

		_, err := f.idem.Replay(ctx, sc)
		programmer("Replay", err)

		tx := f.begin(t)
		claimed, err := f.idem.Claim(ctx, tx, sc)
		programmer("Claim", err)
		if claimed {
			t.Errorf("%s: Claim = true", name)
		}
		programmer("Complete", f.idem.Complete(ctx, tx, sc, resp))
		commit(t, tx)
	}
	if n := f.rows(t); n != 0 {
		t.Errorf("baris key = %d, ingin 0", n)
	}

	// Batasnya dihitung per karakter, sama dengan constraint kolomnya.
	wide := valid
	wide.Endpoint = strings.Repeat("é", 200)
	f.store(t, wide, resp)

	// Tanpa transaksi, dan response berstatus tidak sah.
	if _, err := f.idem.Claim(ctx, nil, valid); err == nil || kind(err) != "" {
		t.Errorf("Claim tanpa transaksi = %v", err)
	}
	if err := f.idem.Complete(ctx, nil, valid, resp); err == nil || kind(err) != "" {
		t.Errorf("Complete tanpa transaksi = %v", err)
	}
	tx := f.begin(t)
	if !f.claim(t, tx, valid) {
		t.Fatal("Claim key baru = false")
	}
	for _, status := range []int{0, 99, 600, -1} {
		if err := f.idem.Complete(ctx, tx, valid, idempotency.Response{Status: status}); err == nil || kind(err) != "" {
			t.Errorf("Complete status %d = %v, ingin galat pemrogram", status, err)
		}
	}
	// Galat tadi tidak merusak transaksinya.
	if err := f.idem.Complete(ctx, tx, valid, resp); err != nil {
		t.Errorf("Complete sesudah status tidak sah = %v", err)
	}
	commit(t, tx)
}

// Complete tanpa Claim tidak menyimpan apa pun.
func TestCompleteNeedsAClaim(t *testing.T) {
	f := setup(t)
	sc := scope(uuid.New(), "kunci-pertama", `{"total":1}`)

	tx := f.begin(t)
	err := f.idem.Complete(context.Background(), tx, sc, idempotency.Response{Status: http.StatusCreated})
	if err == nil || kind(err) != "" {
		t.Errorf("Complete tanpa Claim = %v, ingin galat pemrogram", err)
	}
	commit(t, tx)
	if n := f.rows(t); n != 0 {
		t.Errorf("baris key = %d, ingin 0", n)
	}
}

func TestNew(t *testing.T) {
	if _, err := idempotency.New(nil, idempotency.Options{}); err == nil || err.Error() != "idempotency: pool wajib diisi" {
		t.Errorf("New tanpa pool = %v", err)
	}

	pool := testdb.New(t)
	if _, err := idempotency.New(pool, idempotency.Options{TTL: -time.Second}); err == nil {
		t.Error("New dengan TTL negatif lolos")
	}
	if idempotency.DefaultTTL != 24*time.Hour {
		t.Errorf("DefaultTTL = %s", idempotency.DefaultTTL)
	}

	// Umur key mengikuti Options.TTL; kosong berarti DefaultTTL.
	for key, tc := range map[string]struct {
		ttl, want time.Duration
	}{
		"kunci-ttl-bawaan": {0, idempotency.DefaultTTL},
		"kunci-ttl-pendek": {90 * time.Minute, 90 * time.Minute},
	} {
		s, err := idempotency.New(pool, idempotency.Options{TTL: tc.ttl})
		if err != nil {
			t.Fatal(err)
		}
		f := &fixture{pool: pool, idem: s}
		sc := scope(uuid.New(), key, "1")
		f.store(t, sc, idempotency.Response{Status: http.StatusCreated})

		var seconds float64
		if err := pool.QueryRow(context.Background(), `
			SELECT extract(epoch FROM expires_at - created_at)::float8
			FROM appkit_idempotency_keys WHERE idempotency_key = $1`, key).Scan(&seconds); err != nil {
			t.Fatal(err)
		}
		if seconds != tc.want.Seconds() {
			t.Errorf("%s: umur key = %v detik, ingin %v", key, seconds, tc.want.Seconds())
		}
	}
}

// waitForLock menunggu sampai ada koneksi di database test ini yang menunggu
// kunci.
func (f *fixture) waitForLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := f.pool.QueryRow(context.Background(), `
			SELECT count(*)::int FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("klaim kedua tidak menunggu klaim pertama")
}

// Dua klaim bersamaan atas key yang sama bergiliran: yang kedua menunggu
// transaksi yang pertama selesai. Bila yang pertama commit, yang kedua
// mendapat false; bila dibatalkan, yang kedua mendapat klaimnya.
func TestSecondClaimWaitsForTheFirst(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	for name, firstCommits := range map[string]bool{"yang pertama commit": true, "yang pertama dibatalkan": false} {
		sc := scope(uuid.New(), "kunci-rebutan", `{"total":1}`)
		first := f.begin(t)
		if !f.claim(t, first, sc) {
			t.Fatalf("%s: Claim pertama = false", name)
		}

		type result struct {
			claimed bool
			err     error
		}
		second := f.begin(t)
		done := make(chan result, 1)
		go func() {
			claimed, err := f.idem.Claim(ctx, second, sc)
			done <- result{claimed, err}
		}()

		f.waitForLock(t)
		select {
		case r := <-done:
			t.Fatalf("%s: Claim kedua selesai sebelum yang pertama: %+v", name, r)
		default:
		}

		if firstCommits {
			if err := f.idem.Complete(ctx, first, sc, idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":1}`)}); err != nil {
				t.Fatalf("%s: Complete: %v", name, err)
			}
			commit(t, first)
		} else if err := first.Rollback(ctx); err != nil {
			t.Fatal(err)
		}

		var r result
		select {
		case r = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: Claim kedua tidak pernah selesai", name)
		}
		if r.err != nil || r.claimed == firstCommits {
			t.Fatalf("%s: Claim kedua = %v, %v; ingin %v", name, r.claimed, r.err, !firstCommits)
		}

		want := `{"id":1}`
		if r.claimed {
			want = `{"id":2}`
			if err := f.idem.Complete(ctx, second, sc, idempotency.Response{Status: http.StatusCreated, Body: []byte(want)}); err != nil {
				t.Fatalf("%s: Complete kedua: %v", name, err)
			}
			commit(t, second)
		} else if err := second.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if resp := f.replay(t, sc); resp == nil || string(resp.Body) != want {
			t.Errorf("%s: Replay = %+v, ingin body %s", name, resp, want)
		}
	}
}

// create meniru modul pemanggil: membuat satu "dokumen" produk dengan alur
// Replay → Claim → Complete, seperti di komentar package.
func (f *fixture) create(ctx context.Context, sc idempotency.Scope) (idempotency.Response, error) {
	if resp, err := f.idem.Replay(ctx, sc); err != nil || resp != nil {
		if resp != nil {
			return *resp, nil
		}
		return idempotency.Response{}, err
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return idempotency.Response{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	claimed, err := f.idem.Claim(ctx, tx, sc)
	if err != nil {
		return idempotency.Response{}, err
	}
	if !claimed {
		_ = tx.Rollback(ctx)
		resp, err := f.idem.Replay(ctx, sc)
		if err != nil {
			return idempotency.Response{}, err
		}
		if resp == nil {
			return idempotency.Response{}, errors.New("klaim ditolak tetapi tidak ada yang diputar ulang")
		}
		return *resp, nil
	}

	id := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO documents (id, organization_id) VALUES ($1, $2)`, id, sc.OrganizationID); err != nil {
		return idempotency.Response{}, err
	}
	resp := idempotency.Response{Status: http.StatusCreated, Body: []byte(`{"id":"` + id.String() + `"}`)}
	if err := f.idem.Complete(ctx, tx, sc, resp); err != nil {
		return idempotency.Response{}, err
	}
	return resp, tx.Commit(ctx)
}

// Permintaan yang sama tiba bersamaan: tepat satu yang membuat dokumen, dan
// semuanya menerima response yang sama.
func TestConcurrentRequestsCreateOneDocument(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `CREATE TABLE documents (id uuid PRIMARY KEY, organization_id uuid NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	sc := scope(uuid.New(), "kunci-rebutan", `{"total":1}`)

	const requests = 12
	var wg sync.WaitGroup
	resps := make([]idempotency.Response, requests)
	errs := make([]error, requests)
	for i := range requests {
		wg.Go(func() { resps[i], errs[i] = f.create(ctx, sc) })
	}
	wg.Wait()

	fresh := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("permintaan %d: %v", i, err)
		}
		if !resps[i].Replayed {
			fresh++
		}
		if resps[i].Status != http.StatusCreated || !bytes.Equal(resps[i].Body, resps[0].Body) {
			t.Errorf("permintaan %d menerima %d %s, permintaan 0 menerima %s", i, resps[i].Status, resps[i].Body, resps[0].Body)
		}
	}
	var documents int
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM documents`).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || fresh != 1 {
		t.Errorf("dokumen = %d, response yang bukan putar ulang = %d; ingin 1 dan 1", documents, fresh)
	}

	// Body berbeda dengan key yang sama tidak membuat dokumen kedua.
	_, err := f.create(ctx, scope(sc.OrganizationID, sc.Key, `{"total":2}`))
	if kind(err) != appkit.KindIdempotencyConflict {
		t.Errorf("isi berbeda = %v, ingin galat idempotency_conflict", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM documents`).Scan(&documents); err != nil || documents != 1 {
		t.Errorf("dokumen = %d, %v; ingin 1", documents, err)
	}
}
