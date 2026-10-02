package numbering_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/numbering"
)

// Jenis dokumen "produk" di test ini: satu untuk tiap kebijakan reset.
const (
	invoice = "invoice"
	order   = "sales_order"
	receipt = "receipt"
)

func types() []numbering.Type {
	return []numbering.Type{
		{Key: invoice, Label: "Faktur", Pattern: "INV/{YYYY}/{SEQ:05}", Reset: numbering.ResetYearly},
		{Key: order, Label: "Order Penjualan", Pattern: "SO-{YYYYMM}-{SEQ:04}", Reset: numbering.ResetMonthly},
		{Key: receipt, Label: "Kuitansi", Pattern: "KW{SEQ}", Reset: numbering.ResetNever},
	}
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// fixture memegang yang biasanya dijawab produk: zona waktu tiap organization.
type fixture struct {
	pool    *pgxpool.Pool
	trail   *audit.Service
	numbers *numbering.Service
	jakarta *time.Location

	mu    sync.Mutex
	zones map[uuid.UUID]string
}

func (f *fixture) options() numbering.Options {
	return numbering.Options{
		Types: types(),
		Timezone: func(_ context.Context, org uuid.UUID) string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.zones[org]
		},
	}
}

func setup(t *testing.T, change ...func(*numbering.Options)) *fixture {
	t.Helper()
	f := &fixture{pool: testdb.New(t), jakarta: zone(t, "Asia/Jakarta"), zones: map[uuid.UUID]string{}}
	f.trail = testdb.Trail(t, f.pool)
	opts := f.options()
	for _, c := range change {
		c(&opts)
	}
	s, err := numbering.New(f.pool, f.trail, testdb.Hooks(), opts)
	if err != nil {
		t.Fatal(err)
	}
	f.numbers = s
	return f
}

func (f *fixture) locate(org uuid.UUID, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zones[org] = name
}

// day adalah waktu dokumen di Jakarta, pukul sepuluh pagi.
func (f *fixture) day(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 10, 0, 0, 0, f.jakarta)
}

// now adalah saat ini di Jakarta: layar pengaturan menghitung contoh nomor
// dari lingkup yang sedang berjalan.
func (f *fixture) now() time.Time { return time.Now().In(f.jakarta) }

// dated mengisi tahun dan bulan t ke nomor yang diharapkan, mis.
// "INV/{YYYY}/00001". Sengaja bukan t.Format: angka di dalam nomor terbaca
// sebagai layout waktu.
func dated(t time.Time, number string) string {
	return strings.NewReplacer(
		"{YYYYMM}", t.Format("200601"), "{YYYY}", t.Format("2006"),
		"{YY}", t.Format("06"), "{MM}", t.Format("01"),
	).Replace(number)
}

// attempt menyimpan satu "dokumen": membuka transaksi, mengambil nomornya,
// lalu commit atau batal.
func (f *fixture) attempt(org uuid.UUID, docType string, at time.Time, commit bool) (string, error) {
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	number, err := f.numbers.Next(ctx, tx, org, docType, at)
	if err != nil || !commit {
		return number, err
	}
	return number, tx.Commit(ctx)
}

func (f *fixture) next(t *testing.T, org uuid.UUID, docType string, at time.Time) string {
	t.Helper()
	number, err := f.attempt(org, docType, at, true)
	if err != nil {
		t.Fatalf("Next %s: %v", docType, err)
	}
	return number
}

// expect menyimpan satu dokumen per nomor di want, dan menuntut nomornya.
func (f *fixture) expect(t *testing.T, org uuid.UUID, docType string, at time.Time, want ...string) {
	t.Helper()
	for _, w := range want {
		if got := f.next(t, org, docType, at); got != w {
			t.Errorf("nomor %s = %q, ingin %q", docType, got, w)
		}
	}
}

func (f *fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// admin memegang izin Manage; staff tidak.
func admin(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{
		Organization: org, User: uuid.New(), Permissions: []appkit.Permission{numbering.Manage},
	})
}

func staff(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{Organization: org, User: uuid.New()})
}

func kind(err error) appkit.Kind {
	if e, ok := errors.AsType[*appkit.Error](err); ok {
		return e.Kind
	}
	return ""
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	e, ok := errors.AsType[*appkit.Error](err)
	if !ok || e.Kind != appkit.KindValidation {
		t.Fatalf("galat = %v, ingin galat validasi", err)
	}
	out := map[string]string{}
	for _, f := range e.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func (f *fixture) set(t *testing.T, ctx context.Context, docType string, in numbering.Input) numbering.Scheme {
	t.Helper()
	s, err := f.numbers.SetScheme(ctx, docType, in)
	if err != nil {
		t.Fatalf("SetScheme %s: %v", docType, err)
	}
	return s
}

// schemes mengembalikan skema org per jenis dokumen.
func (f *fixture) schemes(t *testing.T, org uuid.UUID) map[string]numbering.Scheme {
	t.Helper()
	list, err := f.numbers.Schemes(admin(org))
	if err != nil {
		t.Fatalf("Schemes: %v", err)
	}
	out := map[string]numbering.Scheme{}
	for _, s := range list {
		out[s.DocumentType] = s
	}
	return out
}

func TestFormat(t *testing.T) {
	at := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		pattern string
		seq     int64
		want    string
	}{
		{"INV/{YYYY}/{SEQ:05}", 1, "INV/2026/00001"},
		// Lebar adalah batas bawah: nomor yang lebih panjang tidak dipotong.
		{"INV/{YYYY}/{SEQ:05}", 123456, "INV/2026/123456"},
		{"INV/{YYYYMM}/{SEQ:05}", 42, "INV/202609/00042"},
		{"SJ/{YY}/{MM}/{SEQ:03}", 5, "SJ/26/09/005"},
		{"KW{SEQ}", 9, "KW9"},
		{"{SEQ:12}", 7, "000000000007"},
		{"{SEQ:1}-{SEQ:3}", 7, "7-007"},
		{"Faktur № {SEQ}", 3, "Faktur № 3"},
	} {
		got, err := numbering.Format(tc.pattern, at, tc.seq)
		if err != nil || got != tc.want {
			t.Errorf("Format(%q, %d) = %q, %v; ingin %q", tc.pattern, tc.seq, got, err, tc.want)
		}
	}

	for name, pattern := range map[string]string{
		"kosong":                "",
		"tanpa nomor urut":      "INV/{YYYY}",
		"token tak dikenal":     "INV/{DD}/{SEQ}",
		"token huruf kecil":     "INV/{seq}",
		"kurung tidak ditutup":  "INV/{SEQ",
		"kurung tidak dibuka":   "INV/SEQ}",
		"kurung bersarang":      "INV/{{SEQ}}",
		"lebar nol":             "INV/{SEQ:0}",
		"lebar terlalu besar":   "INV/{SEQ:13}",
		"lebar bukan angka":     "INV/{SEQ:lima}",
		"lebar bertanda":        "INV/{SEQ:+5}",
		"lebar kosong":          "INV/{SEQ:}",
		"terlalu panjang":       strings.Repeat("A", 56) + "{SEQ}",
		"baris baru":            "INV\n{SEQ}",
		"token tahun salah eja": "INV/{YYY}/{SEQ}",
		"spasi di dalam kurung": "INV/{ SEQ }",
		"lebar tiga angka":      "INV/{SEQ:005}",
		"nomor urut di luar":    "INV/SEQ",
		"token bulan salah eja": "INV/{YYYY}{M}/{SEQ}",
		"hanya kurung kosong":   "INV/{}/{SEQ}",
		"spasi sebelum lebar":   "INV/{SEQ :05}",
	} {
		if got, err := numbering.Format(pattern, at, 1); err == nil {
			t.Errorf("Format pola %s = %q tanpa galat", name, got)
		}
	}
	// Pola tepat di batas panjang diterima.
	if _, err := numbering.Format(strings.Repeat("A", 55)+"{SEQ}", at, 1); err != nil {
		t.Errorf("pola 60 karakter ditolak: %v", err)
	}
	if got, err := numbering.Format("KW{SEQ}", at, 0); err == nil {
		t.Errorf("nomor urut 0 = %q tanpa galat", got)
	}
}

// Daftar token untuk layar harus sama dengan yang dikenal Format.
func TestTokens(t *testing.T) {
	at := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	tokens := numbering.Tokens()
	if len(tokens) != 6 {
		t.Errorf("token = %+v", tokens)
	}
	for _, token := range tokens {
		if token.Meaning == "" {
			t.Errorf("token %s tanpa arti", token.Token)
		}
		if got, err := numbering.Format(token.Token+"/{SEQ}", at, 1); err != nil || strings.Contains(got, "{") {
			t.Errorf("token %s tidak dikenal Format: %q, %v", token.Token, got, err)
		}
	}
	raw, _ := json.Marshal(tokens[0])
	if string(raw) != `{"token":"{YYYY}","meaning":"Tahun empat angka, mis. 2026."}` {
		t.Errorf("JSON token = %s", raw)
	}
}

// Bawaan ada di kode: berlaku di setiap organization tanpa baris apa pun, dan
// tanpa langkah menyiapkan organization baru.
func TestNextUsesTheDefaultScheme(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	at := f.day(2026, time.September, 11)

	f.expect(t, org, invoice, at, "INV/2026/00001", "INV/2026/00002", "INV/2026/00003")
	// Tiap jenis dokumen punya urutannya sendiri.
	f.expect(t, org, order, at, "SO-202609-0001", "SO-202609-0002")
	f.expect(t, org, receipt, at, "KW1", "KW2")
	f.expect(t, org, invoice, at, "INV/2026/00004")

	if n := f.count(t, "appkit_number_schemes"); n != 0 {
		t.Errorf("memakai bawaan membuat %d baris skema", n)
	}
}

func TestReset(t *testing.T) {
	f := setup(t)
	org := uuid.New()

	// Tahunan: kembali ke 1 di tahun baru, bukan di bulan baru.
	f.expect(t, org, invoice, f.day(2026, time.November, 30), "INV/2026/00001")
	f.expect(t, org, invoice, f.day(2026, time.December, 31), "INV/2026/00002")
	f.expect(t, org, invoice, f.day(2027, time.January, 1), "INV/2027/00001", "INV/2027/00002")
	// Dokumen bertanggal mundur melanjutkan urutan tahunnya sendiri.
	f.expect(t, org, invoice, f.day(2026, time.December, 31), "INV/2026/00003")

	// Bulanan: kembali ke 1 di bulan baru, juga saat tahunnya berganti.
	f.expect(t, org, order, f.day(2026, time.November, 30), "SO-202611-0001", "SO-202611-0002")
	f.expect(t, org, order, f.day(2026, time.December, 1), "SO-202612-0001")
	f.expect(t, org, order, f.day(2027, time.January, 1), "SO-202701-0001")
	f.expect(t, org, order, f.day(2027, time.December, 1), "SO-202712-0001")
	f.expect(t, org, order, f.day(2026, time.December, 20), "SO-202612-0002")

	// Tidak pernah: terus naik melewati tahun.
	f.expect(t, org, receipt, f.day(2026, time.December, 31), "KW1", "KW2")
	f.expect(t, org, receipt, f.day(2027, time.January, 1), "KW3")
	f.expect(t, org, receipt, f.day(2031, time.June, 1), "KW4")
}

// Tahun dan bulan pada nomor, dan lingkup reset-nya, mengikuti waktu setempat
// organization, bukan UTC.
func TestYearFollowsTheOrganizationTimezone(t *testing.T) {
	f := setup(t)
	// 31 Desember 2026 pukul 18.00 UTC sudah 1 Januari 2027 pukul 01.00 di
	// Jakarta, dan masih 31 Desember siang di New York.
	at := time.Date(2026, time.December, 31, 18, 0, 0, 0, time.UTC)

	jakarta, newYork, unknown, unset := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	f.locate(jakarta, "Asia/Jakarta")
	f.locate(newYork, "America/New_York")
	// Zona yang tidak dikenal tidak menggagalkan pembuatan dokumen: yang
	// dipakai zona bawaan.
	f.locate(unknown, "Mars/Olympus_Mons")

	f.expect(t, jakarta, invoice, at, "INV/2027/00001")
	f.expect(t, newYork, invoice, at, "INV/2026/00001")
	f.expect(t, unknown, invoice, at, "INV/2027/00001")
	f.expect(t, unset, invoice, at, "INV/2027/00001")
	f.expect(t, jakarta, order, at, "SO-202701-0001")
	f.expect(t, newYork, order, at, "SO-202612-0001")

	// Lingkup reset-nya ikut: satu menit sebelum tengah malam Jakarta masih
	// urutan 2026, dan tengah malamnya sudah urutan 2027.
	f.expect(t, jakarta, invoice, time.Date(2026, time.December, 31, 16, 59, 0, 0, time.UTC), "INV/2026/00001")
	f.expect(t, jakarta, invoice, time.Date(2026, time.December, 31, 17, 0, 0, 0, time.UTC), "INV/2027/00002")
	// Zona waktu yang dibawa at tidak berpengaruh; yang dipakai zona organization.
	f.expect(t, jakarta, invoice, at.In(zone(t, "America/New_York")), "INV/2027/00003")
}

// Options.Timezone boleh kosong, dan zona bawaannya dapat diganti.
func TestDefaultTimezone(t *testing.T) {
	at := time.Date(2026, time.December, 31, 18, 0, 0, 0, time.UTC)
	org := uuid.New()

	f := setup(t, func(o *numbering.Options) { o.Timezone = nil })
	f.expect(t, org, invoice, at, "INV/2027/00001")

	f = setup(t, func(o *numbering.Options) { o.Timezone, o.DefaultTimezone = nil, "UTC" })
	f.expect(t, org, invoice, at, "INV/2026/00001")
}

// Yang salah di sini adalah kode pemanggilnya, bukan isian pengguna: galatnya
// bukan *appkit.Error, dan tidak ada nomor yang terpakai.
func TestNextRejectsProgrammerErrors(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	at := f.day(2026, time.September, 11)
	ctx := context.Background()

	for _, docType := range []string{"delivery_note", "", "INVOICE"} {
		if got, err := f.attempt(org, docType, at, true); err == nil || kind(err) != "" {
			t.Errorf("Next jenis %q = %q, %v; ingin galat pemrogram", docType, got, err)
		}
	}
	if got, err := f.attempt(uuid.Nil, invoice, at, true); err == nil {
		t.Errorf("Next tanpa organization = %q tanpa galat", got)
	}
	if got, err := f.attempt(org, invoice, time.Time{}, true); err == nil {
		t.Errorf("Next tanpa waktu dokumen = %q tanpa galat", got)
	}
	if got, err := f.numbers.Next(ctx, nil, org, invoice, at); err == nil {
		t.Errorf("Next tanpa transaksi = %q tanpa galat", got)
	}

	if n := f.count(t, "appkit_number_counters"); n != 0 {
		t.Errorf("panggilan yang ditolak memakai nomor: %d baris counter", n)
	}
	f.expect(t, org, invoice, at, "INV/2026/00001")
}

// TANPA NOMOR LOMPAT: nomor diambil di transaksi dokumennya, jadi dokumen yang
// batal disimpan mengembalikan nomornya dan dokumen berikutnya memakainya.
func TestRolledBackNumberIsReused(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	at := f.day(2026, time.September, 11)

	// Nomor pertama sebuah lingkup, yang counter-nya belum punya baris.
	for range 2 {
		if got, err := f.attempt(org, invoice, at, false); err != nil || got != "INV/2026/00001" {
			t.Errorf("dokumen yang batal = %q, %v", got, err)
		}
	}
	f.expect(t, org, invoice, at, "INV/2026/00001", "INV/2026/00002")

	if got, err := f.attempt(org, invoice, at, false); err != nil || got != "INV/2026/00003" {
		t.Errorf("dokumen yang batal = %q, %v", got, err)
	}
	f.expect(t, org, invoice, at, "INV/2026/00003")

	// Satu transaksi yang mengambil beberapa nomor mengembalikan semuanya.
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"INV/2026/00004", "INV/2026/00005"} {
		if got, err := f.numbers.Next(ctx, tx, org, invoice, at); err != nil || got != want {
			t.Errorf("nomor di satu transaksi = %q, %v; ingin %q", got, err, want)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	f.expect(t, org, invoice, at, "INV/2026/00004")
}

// TANPA NOMOR KEMBAR: dokumen yang disimpan bersamaan mendapat nomor yang
// berbeda-beda, dan bersama-sama tepat 1..N.
func TestConcurrentSavesGetDistinctNumbers(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	at := f.day(2026, time.September, 11)

	const n = 24
	var wg sync.WaitGroup
	numbers, errs := make([]string, n), make([]error, n)
	for i := range n {
		wg.Go(func() { numbers[i], errs[i] = f.attempt(org, invoice, at, true) })
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("simpan bersamaan: %v", err)
		}
	}
	slices.Sort(numbers)
	for i, got := range numbers {
		if want := fmt.Sprintf("INV/2026/%05d", i+1); got != want {
			t.Fatalf("nomor ke-%d = %q, ingin %q; seluruhnya %v", i+1, got, want, numbers)
		}
	}
}

// Sebagian dokumen yang disimpan bersamaan batal. Yang tersimpan tetap tepat
// 1..M: tidak kembar, dan nomor yang batal tidak meninggalkan lubang.
func TestConcurrentRollbacksLeaveNoGap(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	at := f.day(2026, time.September, 11)

	const n = 30
	var wg sync.WaitGroup
	numbers, errs := make([]string, n), make([]error, n)
	for i := range n {
		wg.Go(func() { numbers[i], errs[i] = f.attempt(org, receipt, at, i%3 != 0) })
	}
	wg.Wait()

	var saved []string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("simpan bersamaan: %v", err)
		}
		if i%3 != 0 {
			saved = append(saved, numbers[i])
		}
	}
	want := make([]string, len(saved))
	for i := range want {
		want[i] = fmt.Sprintf("KW%d", i+1)
	}
	slices.Sort(saved)
	slices.Sort(want)
	if !slices.Equal(saved, want) {
		t.Errorf("nomor tersimpan = %v, ingin %v", saved, want)
	}
	f.expect(t, org, receipt, at, fmt.Sprintf("KW%d", len(saved)+1))
}

// Harga yang diterima: dokumen kedua berjenis sama di organization yang sama
// menunggu sampai transaksi yang pertama selesai. Jenis dokumen lain dan
// organization lain tidak ikut menunggu.
func TestSecondSaveWaitsForTheFirst(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	at := f.day(2026, time.September, 11)
	ctx := context.Background()

	first, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	if got, err := f.numbers.Next(ctx, first, org, invoice, at); err != nil || got != "INV/2026/00001" {
		t.Fatalf("dokumen pertama = %q, %v", got, err)
	}

	type result struct {
		number string
		err    error
	}
	second := make(chan result, 1)
	go func() {
		number, err := f.attempt(org, invoice, at, true)
		second <- result{number, err}
	}()

	// Selagi yang pertama belum selesai, jenis dokumen lain dan organization
	// lain tetap berjalan.
	f.expect(t, org, receipt, at, "KW1")
	f.expect(t, uuid.New(), invoice, at, "INV/2026/00001")

	select {
	case got := <-second:
		t.Fatalf("dokumen kedua tidak menunggu yang pertama: %q, %v", got.number, got.err)
	case <-time.After(300 * time.Millisecond):
	}

	// Yang pertama batal: nomornya kembali, dan dipakai dokumen yang menunggu.
	if err := first.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-second; got.err != nil || got.number != "INV/2026/00001" {
		t.Errorf("dokumen kedua = %q, %v; ingin nomor yang dikembalikan", got.number, got.err)
	}
}

// Jenis dokumen didaftarkan kode: rilis berikutnya menambah jenis tanpa
// migrasi, dan baris milik jenis yang sudah tidak terdaftar diabaikan.
func TestTypesLiveInCode(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	at := f.now()
	f.expect(t, org, receipt, at, "KW1")
	f.set(t, admin(org), receipt, numbering.Input{Pattern: "KWT-{SEQ:03}", ResetPolicy: numbering.ResetNever})

	opts := f.options()
	opts.Types = []numbering.Type{
		{Key: invoice, Label: "Faktur", Pattern: "INV/{YYYY}/{SEQ:05}", Reset: numbering.ResetYearly},
		{Key: "delivery_note", Label: "Surat Jalan", Pattern: "SJ/{YY}{MM}/{SEQ:04}", Reset: numbering.ResetMonthly},
	}
	release, err := numbering.New(f.pool, f.trail, testdb.Hooks(), opts)
	if err != nil {
		t.Fatal(err)
	}
	f.numbers = release

	f.expect(t, org, "delivery_note", at, dated(at, "SJ/{YY}{MM}/0001"))
	if _, err := f.attempt(org, receipt, at, true); err == nil {
		t.Error("jenis yang sudah tidak terdaftar masih dinomori")
	}
	list, err := release.Schemes(admin(org))
	if err != nil || len(list) != 2 || list[0].DocumentType != invoice || list[1].DocumentType != "delivery_note" {
		t.Errorf("Schemes rilis berikutnya = %+v, %v", list, err)
	}
}

func TestSchemesReturnsDefaults(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	now := f.now()

	list, err := f.numbers.Schemes(admin(org))
	if err != nil {
		t.Fatalf("Schemes: %v", err)
	}
	// Satu per jenis dokumen, menurut urutan pendaftarannya.
	want := []numbering.Scheme{
		{DocumentType: invoice, Label: "Faktur", Pattern: "INV/{YYYY}/{SEQ:05}", ResetPolicy: numbering.ResetYearly,
			NextNumber: dated(now, "INV/{YYYY}/00001")},
		{DocumentType: order, Label: "Order Penjualan", Pattern: "SO-{YYYYMM}-{SEQ:04}", ResetPolicy: numbering.ResetMonthly,
			NextNumber: dated(now, "SO-{YYYYMM}-0001")},
		{DocumentType: receipt, Label: "Kuitansi", Pattern: "KW{SEQ}", ResetPolicy: numbering.ResetNever, NextNumber: "KW1"},
	}
	if !slices.Equal(list, want) {
		t.Errorf("Schemes = %+v\ningin %+v", list, want)
	}
	// Bentuk JSON-nya tetap lengkap: klien tidak perlu menebak field yang hilang.
	raw, _ := json.Marshal(list[0])
	for _, field := range []string{
		`"document_type":"invoice"`, `"label":"Faktur"`, `"pattern":"INV/{YYYY}/{SEQ:05}"`, `"reset_policy":"yearly"`,
		`"custom":false`, `"next_number":"INV/`, `"version":0`, `"updated_at":null`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("JSON tidak memuat %s: %s", field, raw)
		}
	}
}

// Contoh nomor berikutnya dihitung dari lingkup yang sedang berjalan menurut
// kebijakan MASING-MASING jenis dokumen, dan tidak memakai nomor.
func TestNextNumberIsAnExample(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	now := f.now()

	f.expect(t, org, invoice, now, dated(now, "INV/{YYYY}/00001"), dated(now, "INV/{YYYY}/00002"))
	f.next(t, org, order, now)
	for range 3 {
		f.next(t, org, receipt, now)
	}
	// Nomor urut lingkup lain tidak ikut dihitung: faktur tahun lalu dan tahun
	// depan, order bulan lalu, dan nomor milik organization lain.
	for range 4 {
		f.next(t, org, invoice, now.AddDate(-1, 0, 0))
		f.next(t, org, invoice, now.AddDate(1, 0, 0))
		f.next(t, org, order, time.Date(now.Year(), now.Month(), 1, 10, 0, 0, 0, f.jakarta).AddDate(0, -1, 0))
		f.next(t, uuid.New(), receipt, now)
	}

	want := map[string]string{
		invoice: dated(now, "INV/{YYYY}/00003"),
		order:   dated(now, "SO-{YYYYMM}-0002"),
		receipt: "KW4",
	}
	for range 2 {
		for docType, s := range f.schemes(t, org) {
			if s.NextNumber != want[docType] {
				t.Errorf("contoh nomor %s = %q, ingin %q", docType, s.NextNumber, want[docType])
			}
		}
	}
	// Melihat contoh tidak memakai nomor: dokumen berikutnya mendapat tepat
	// nomor yang dicontohkan.
	for docType, number := range want {
		f.expect(t, org, docType, now, number)
	}
}

func TestSetScheme(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	now := f.now()
	f.expect(t, org, invoice, now, dated(now, "INV/{YYYY}/00001"), dated(now, "INV/{YYYY}/00002"))

	s := f.set(t, ctx, invoice, numbering.Input{Pattern: "  FK-{YY}-{SEQ:03} ", ResetPolicy: " yearly "})
	want := numbering.Scheme{
		DocumentType: invoice, Label: "Faktur", Pattern: "FK-{YY}-{SEQ:03}", ResetPolicy: numbering.ResetYearly,
		Custom: true, NextNumber: dated(now, "FK-{YY}-003"), Version: 1, UpdatedAt: s.UpdatedAt,
	}
	if s != want || s.UpdatedAt == nil {
		t.Errorf("skema tersimpan = %+v\ningin %+v", s, want)
	}
	if got := f.schemes(t, org)[invoice]; got.Pattern != want.Pattern || !got.Custom || got.Version != 1 ||
		got.UpdatedAt == nil || !got.UpdatedAt.Equal(*s.UpdatedAt) || got.NextNumber != want.NextNumber {
		t.Errorf("Schemes sesudah simpan = %+v", got)
	}

	// Mengganti pola tidak menyentuh nomor urut: nomor yang sudah terbit tetap,
	// dan nomor berikutnya melanjutkan urutannya dengan bentuk baru.
	f.expect(t, org, invoice, now, dated(now, "FK-{YY}-003"), dated(now, "FK-{YY}-004"))

	s = f.set(t, ctx, invoice, numbering.Input{Pattern: "FK-{YY}-{SEQ:03}", ResetPolicy: numbering.ResetYearly, Version: s.Version})
	if s.Version != 1 {
		t.Errorf("simpan tanpa perubahan menaikkan version: %+v", s)
	}
	s = f.set(t, ctx, invoice, numbering.Input{Pattern: "F{YYYY}.{SEQ}", ResetPolicy: numbering.ResetYearly, Version: s.Version})
	if s.Version != 2 || s.NextNumber != dated(now, "F{YYYY}.5") {
		t.Errorf("simpan kedua = %+v", s)
	}
	f.expect(t, org, invoice, now, dated(now, "F{YYYY}.5"))

	// Jenis dokumen lain tidak ikut berubah, dan barisnya hanya ada untuk yang
	// diubah.
	all := f.schemes(t, org)
	if all[order].Custom || all[receipt].Custom || all[order].Pattern != "SO-{YYYYMM}-{SEQ:04}" {
		t.Errorf("jenis dokumen lain ikut berubah: %+v", all)
	}
	if n := f.count(t, "appkit_number_schemes"); n != 1 {
		t.Errorf("baris skema = %d, ingin 1", n)
	}
}

// Simpan yang tidak mengubah bawaan tidak membuat baris: organization itu
// tetap mengikuti bawaan dari kode.
func TestSavingTheDefaultUnchangedStoresNothing(t *testing.T) {
	f := setup(t)
	org := uuid.New()

	s := f.set(t, admin(org), invoice, numbering.Input{Pattern: "INV/{YYYY}/{SEQ:05}", ResetPolicy: numbering.ResetYearly})
	if s.Custom || s.Version != 0 || s.UpdatedAt != nil {
		t.Errorf("skema = %+v, ingin tetap bawaan", s)
	}
	if n := f.count(t, "appkit_number_schemes"); n != 0 {
		t.Errorf("baris skema = %d, ingin 0", n)
	}
	if actions := testdb.Actions(t, f.trail, org); len(actions) != 0 {
		t.Errorf("simpan tanpa perubahan tercatat: %v", actions)
	}
}

// Dua administrator membuka layar yang sama: yang menyimpan belakangan
// ditolak, bukan menimpa diam-diam.
func TestSetSchemeRejectsStaleVersion(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	in := numbering.Input{Pattern: "FK/{YYYY}/{SEQ:04}", ResetPolicy: numbering.ResetYearly}

	// Selama bawaan yang berlaku, version-nya 0.
	for _, version := range []int{1, 99} {
		in.Version = version
		if _, err := f.numbers.SetScheme(ctx, invoice, in); kind(err) != appkit.KindConflict {
			t.Errorf("simpan pertama dengan version %d = %v, ingin konflik", version, err)
		}
	}
	in.Version = 0
	s := f.set(t, ctx, invoice, in)

	for _, version := range []int{0, 2, 99} {
		_, err := f.numbers.SetScheme(ctx, invoice, numbering.Input{Pattern: "TIMPA/{YYYY}/{SEQ}", ResetPolicy: numbering.ResetYearly, Version: version})
		if kind(err) != appkit.KindConflict {
			t.Errorf("simpan dengan version %d = %v, ingin konflik", version, err)
		}
	}
	// Version yang basi ditolak juga bila isinya kebetulan sama.
	in.Version = 0
	if _, err := f.numbers.SetScheme(ctx, invoice, in); kind(err) != appkit.KindConflict {
		t.Errorf("simpan isi yang sama dengan version basi = %v, ingin konflik", err)
	}

	if got := f.schemes(t, org)[invoice]; got.Pattern != "FK/{YYYY}/{SEQ:04}" || got.Version != s.Version {
		t.Errorf("simpan yang ditolak mengubah skema: %+v", got)
	}
	if actions := testdb.Actions(t, f.trail, org); len(actions) != 1 {
		t.Errorf("simpan yang ditolak tercatat: %v", actions)
	}
}

func TestSetSchemeValidation(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)

	for name, tc := range map[string]struct {
		in    numbering.Input
		field string
	}{
		"pola kosong":              {numbering.Input{Pattern: "  ", ResetPolicy: numbering.ResetNever}, "pattern"},
		"tanpa nomor urut":         {numbering.Input{Pattern: "INV/{YYYY}", ResetPolicy: numbering.ResetYearly}, "pattern"},
		"token tak dikenal":        {numbering.Input{Pattern: "INV/{TAHUN}/{SEQ:05}", ResetPolicy: numbering.ResetNever}, "pattern"},
		"kurung tidak berpasangan": {numbering.Input{Pattern: "INV/{SEQ", ResetPolicy: numbering.ResetNever}, "pattern"},
		"lebar terlalu besar":      {numbering.Input{Pattern: "INV/{SEQ:13}", ResetPolicy: numbering.ResetNever}, "pattern"},
		"pola terlalu panjang":     {numbering.Input{Pattern: strings.Repeat("A", 56) + "{SEQ}", ResetPolicy: numbering.ResetNever}, "pattern"},
		"pola berbaris baru":       {numbering.Input{Pattern: "INV\n{SEQ}", ResetPolicy: numbering.ResetNever}, "pattern"},
		"kebijakan tak dikenal":    {numbering.Input{Pattern: "INV/{YYYY}/{SEQ}", ResetPolicy: "weekly"}, "reset_policy"},
		"kebijakan kosong":         {numbering.Input{Pattern: "INV/{YYYY}/{SEQ}"}, "reset_policy"},
		"kebijakan huruf besar":    {numbering.Input{Pattern: "INV/{YYYY}/{SEQ}", ResetPolicy: "YEARLY"}, "reset_policy"},
		// Urutannya mulai lagi sedangkan nomor yang tercetak tidak berubah: dua
		// dokumen mendapat nomor yang sama.
		"tahunan tanpa tahun":       {numbering.Input{Pattern: "INV/{SEQ:05}", ResetPolicy: numbering.ResetYearly}, "pattern"},
		"tahunan hanya bulan":       {numbering.Input{Pattern: "INV/{MM}/{SEQ:05}", ResetPolicy: numbering.ResetYearly}, "pattern"},
		"bulanan tanpa bulan":       {numbering.Input{Pattern: "INV/{YYYY}/{SEQ:05}", ResetPolicy: numbering.ResetMonthly}, "pattern"},
		"bulanan tanpa tahun":       {numbering.Input{Pattern: "INV/{MM}/{SEQ:05}", ResetPolicy: numbering.ResetMonthly}, "pattern"},
		"bulanan tanpa keduanya":    {numbering.Input{Pattern: "INV/{SEQ:05}", ResetPolicy: numbering.ResetMonthly}, "pattern"},
		"version negatif":           {numbering.Input{Pattern: "INV/{YYYY}/{SEQ}", ResetPolicy: numbering.ResetYearly, Version: -1}, "version"},
		"pola dan kebijakan (pola)": {numbering.Input{Pattern: "INV", ResetPolicy: "weekly"}, "pattern"},
		"pola dan kebijakan":        {numbering.Input{Pattern: "INV", ResetPolicy: "weekly"}, "reset_policy"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.numbers.SetScheme(ctx, invoice, tc.in)
			if fields := fieldErrors(t, err); fields[tc.field] == "" {
				t.Errorf("tidak ada galat untuk %s: %v", tc.field, fields)
			}
		})
	}

	// Yang gagal validasi tidak menyimpan apa pun, termasuk catatannya.
	if n := f.count(t, "appkit_number_schemes"); n != 0 {
		t.Errorf("isian yang tidak sah tersimpan: %d baris", n)
	}
	if actions := testdb.Actions(t, f.trail, org); len(actions) != 0 {
		t.Errorf("isian yang tidak sah tercatat: %v", actions)
	}

	// Pola yang memuat masa reset-nya diterima, dengan token mana pun.
	version := 0
	for _, in := range []numbering.Input{
		{Pattern: "INV/{YY}/{SEQ}", ResetPolicy: numbering.ResetYearly},
		{Pattern: "INV/{YYYYMM}/{SEQ}", ResetPolicy: numbering.ResetYearly},
		{Pattern: "INV/{YYYYMM}/{SEQ:02}", ResetPolicy: numbering.ResetMonthly},
		{Pattern: "INV/{MM}.{YY}/{SEQ}", ResetPolicy: numbering.ResetMonthly},
		{Pattern: "INV/{YYYY}/{MM}/{SEQ}", ResetPolicy: numbering.ResetMonthly},
		{Pattern: "{SEQ:12}", ResetPolicy: numbering.ResetNever},
		{Pattern: strings.Repeat("A", 55) + "{SEQ}", ResetPolicy: numbering.ResetNever},
	} {
		in.Version = version
		s, err := f.numbers.SetScheme(ctx, invoice, in)
		if err != nil {
			t.Fatalf("pola %q dengan reset %s ditolak: %v", in.Pattern, in.ResetPolicy, err)
		}
		version = s.Version
	}
}

func TestSetSchemeUnknownType(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	in := numbering.Input{Pattern: "SJ/{YYYY}/{SEQ}", ResetPolicy: numbering.ResetYearly}

	for _, docType := range []string{"delivery_note", "", "INVOICE", "invoice ", uuid.NewString()} {
		if _, err := f.numbers.SetScheme(admin(org), docType, in); kind(err) != appkit.KindNotFound {
			t.Errorf("SetScheme jenis %q = %v, ingin tidak ditemukan", docType, err)
		}
	}
	if n := f.count(t, "appkit_number_schemes"); n != 0 {
		t.Errorf("jenis yang tidak dikenal tersimpan: %d baris", n)
	}
}

// Izin ditegakkan service, dan galat dari pengait diteruskan apa adanya.
func TestOnlyManageMayChange(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	in := numbering.Input{Pattern: "FK/{YYYY}/{SEQ:04}", ResetPolicy: numbering.ResetYearly}

	for name, ctx := range map[string]context.Context{"tanpa izin": staff(org), "tanpa sesi": context.Background()} {
		want := testdb.ErrDenied
		if name == "tanpa sesi" {
			want = testdb.ErrNoSession
		}
		if _, err := f.numbers.Schemes(ctx); !errors.Is(err, want) {
			t.Errorf("Schemes %s = %v", name, err)
		}
		if _, err := f.numbers.SetScheme(ctx, invoice, in); !errors.Is(err, want) {
			t.Errorf("SetScheme %s = %v", name, err)
		}
		// Yang tidak berhak tidak diberi tahu jenis dokumen mana yang dikenal,
		// atau isiannya salah di mana.
		if _, err := f.numbers.SetScheme(ctx, "delivery_note", numbering.Input{}); !errors.Is(err, want) {
			t.Errorf("SetScheme jenis tak dikenal %s = %v", name, err)
		}
	}

	// Pemegang izin tanpa identitas pengguna tidak dapat mengubah: perubahan
	// tanpa pelaku tidak pernah tersimpan.
	anonymous := testdb.With(context.Background(), testdb.Session{Organization: org, Permissions: []appkit.Permission{numbering.Manage}})
	if _, err := f.numbers.SetScheme(anonymous, invoice, in); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("SetScheme tanpa pengguna = %v", err)
	}

	if got := f.schemes(t, org)[invoice]; got.Custom || got.Pattern != "INV/{YYYY}/{SEQ:05}" {
		t.Errorf("percobaan tanpa izin mengubah skema: %+v", got)
	}
	if n := f.count(t, "appkit_number_schemes"); n != 0 {
		t.Errorf("percobaan tanpa izin tersimpan: %d baris", n)
	}
}

// Nomor urut dan skema milik satu organization tidak pernah terlihat atau
// berubah dari organization lain.
func TestTenantIsolation(t *testing.T) {
	f := setup(t)
	a, b := uuid.New(), uuid.New()
	now := f.now()

	// Nomor urutnya terpisah.
	f.expect(t, a, invoice, now, dated(now, "INV/{YYYY}/00001"), dated(now, "INV/{YYYY}/00002"), dated(now, "INV/{YYYY}/00003"))
	f.expect(t, b, invoice, now, dated(now, "INV/{YYYY}/00001"))

	// Skema A tidak ada bagi organization B, di jalur mana pun.
	sa := f.set(t, admin(a), invoice, numbering.Input{Pattern: "A-{YYYY}-{SEQ:03}", ResetPolicy: numbering.ResetYearly})
	if got := f.schemes(t, b)[invoice]; got.Custom || got.Version != 0 || got.UpdatedAt != nil ||
		got.Pattern != "INV/{YYYY}/{SEQ:05}" || got.NextNumber != dated(now, "INV/{YYYY}/00002") {
		t.Errorf("organization B melihat skema A: %+v", got)
	}
	f.expect(t, b, invoice, now, dated(now, "INV/{YYYY}/00002"))
	f.expect(t, a, invoice, now, dated(now, "A-{YYYY}-004"))

	// Version skema A tidak berlaku di B: bagi B skemanya masih bawaan.
	_, err := f.numbers.SetScheme(admin(b), invoice, numbering.Input{Pattern: "DIBAJAK-{YYYY}-{SEQ}", ResetPolicy: numbering.ResetYearly, Version: sa.Version})
	if kind(err) != appkit.KindConflict {
		t.Errorf("SetScheme B dengan version A = %v, ingin konflik", err)
	}
	if actions := testdb.Actions(t, f.trail, b); len(actions) != 0 {
		t.Errorf("percobaan organization B tercatat: %v", actions)
	}

	// B mengubah skemanya sendiri; A tidak ikut berubah.
	sb := f.set(t, admin(b), invoice, numbering.Input{Pattern: "B/{SEQ}", ResetPolicy: numbering.ResetNever})
	if sb.Version != 1 || sb.NextNumber != "B/3" {
		t.Errorf("skema B = %+v", sb)
	}
	if got := f.schemes(t, a)[invoice]; got.Pattern != "A-{YYYY}-{SEQ:03}" || got.Version != sa.Version ||
		got.ResetPolicy != numbering.ResetYearly || got.NextNumber != dated(now, "A-{YYYY}-005") {
		t.Errorf("skema A berubah oleh organization B: %+v", got)
	}
	f.expect(t, a, invoice, now, dated(now, "A-{YYYY}-005"))
	f.expect(t, b, invoice, now, "B/3")

	if actions := testdb.Actions(t, f.trail, a); len(actions) != 1 {
		t.Errorf("catatan A = %v", actions)
	}
	if actions := testdb.Actions(t, f.trail, b); len(actions) != 1 {
		t.Errorf("catatan B = %v", actions)
	}
}

// Mengganti kebijakan reset memindahkan nomor urut ke lingkup lain. Urutan
// yang sedang berjalan ikut dibawa: tanpa itu, lingkup barunya mulai dari 1
// dan nomor yang sudah terbit keluar lagi.
func TestResetPolicyChangeContinuesTheSequence(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	now := f.now()
	pattern := "INV/{YYYY}/{SEQ:05}"
	f.expect(t, org, invoice, now, dated(now, "INV/{YYYY}/00001"), dated(now, "INV/{YYYY}/00002"), dated(now, "INV/{YYYY}/00003"))

	// Tahunan ke tidak pernah, dengan pola yang sama: INV/…/00001 sudah terbit.
	s := f.set(t, ctx, invoice, numbering.Input{Pattern: pattern, ResetPolicy: numbering.ResetNever})
	if s.NextNumber != dated(now, "INV/{YYYY}/00004") {
		t.Errorf("contoh nomor sesudah ganti kebijakan = %q", s.NextNumber)
	}
	f.expect(t, org, invoice, now, dated(now, "INV/{YYYY}/00004"))

	s = f.set(t, ctx, invoice, numbering.Input{Pattern: "INV/{YYYYMM}/{SEQ:05}", ResetPolicy: numbering.ResetMonthly, Version: s.Version})
	f.expect(t, org, invoice, now, dated(now, "INV/{YYYYMM}/00005"))

	// Kembali ke tahunan: lingkup tahun ini berhenti di 3, tetapi urutannya
	// sudah sampai 5.
	s = f.set(t, ctx, invoice, numbering.Input{Pattern: pattern, ResetPolicy: numbering.ResetYearly, Version: s.Version})
	if s.NextNumber != dated(now, "INV/{YYYY}/00006") {
		t.Errorf("contoh nomor sesudah kembali ke tahunan = %q", s.NextNumber)
	}
	f.expect(t, org, invoice, now, dated(now, "INV/{YYYY}/00006"))

	// Sesudah perpindahan, reset-nya tetap menurut kebijakan baru: tahun depan
	// kembali ke 1.
	f.expect(t, org, invoice, now.AddDate(1, 0, 0), dated(now.AddDate(1, 0, 0), "INV/{YYYY}/00001"))

	// Lingkup tujuan yang sudah lebih jauh tidak pernah diturunkan. Order
	// (bulanan) baru sampai 2 bulan ini, sedangkan lingkup tahun ini sudah 40.
	f.expect(t, org, order, now, dated(now, "SO-{YYYYMM}-0001"), dated(now, "SO-{YYYYMM}-0002"))
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO appkit_number_counters (organization_id, document_type, scope, last_value)
		VALUES ($1, $2, $3, 40)`, org, order, now.Format("2006")); err != nil {
		t.Fatal(err)
	}
	s = f.set(t, ctx, order, numbering.Input{Pattern: "SO-{YYYY}-{SEQ:04}", ResetPolicy: numbering.ResetYearly})
	if s.NextNumber != dated(now, "SO-{YYYY}-0041") {
		t.Errorf("contoh nomor order = %q, ingin melanjutkan 40", s.NextNumber)
	}
	f.expect(t, org, order, now, dated(now, "SO-{YYYY}-0041"))

	// Jenis yang belum pernah dinomori tidak punya urutan untuk dibawa.
	s = f.set(t, ctx, receipt, numbering.Input{Pattern: "KW/{YY}/{SEQ}", ResetPolicy: numbering.ResetYearly})
	if s.NextNumber != dated(now, "KW/{YY}/1") {
		t.Errorf("contoh nomor kuitansi = %q", s.NextNumber)
	}
}

// Skema tidak berganti di tengah dokumen yang sedang disimpan: SetScheme
// menunggu, sehingga nomor urut yang dibawanya sudah memuat dokumen itu.
func TestSchemeChangeWaitsForSavesInFlight(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	now := f.now()
	ctx := context.Background()

	doc, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = doc.Rollback(ctx) }()
	if got, err := f.numbers.Next(ctx, doc, org, invoice, now); err != nil || got != dated(now, "INV/{YYYY}/00001") {
		t.Fatalf("dokumen = %q, %v", got, err)
	}

	type result struct {
		scheme numbering.Scheme
		err    error
	}
	changed := make(chan result, 1)
	go func() {
		s, err := f.numbers.SetScheme(admin(org), invoice, numbering.Input{Pattern: "INV/{YYYY}/{SEQ:05}", ResetPolicy: numbering.ResetNever})
		changed <- result{s, err}
	}()
	select {
	case got := <-changed:
		t.Fatalf("SetScheme tidak menunggu dokumen yang sedang disimpan: %+v, %v", got.scheme, got.err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := doc.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-changed
	if got.err != nil || got.scheme.NextNumber != dated(now, "INV/{YYYY}/00002") {
		t.Errorf("SetScheme = %+v, %v; ingin melanjutkan nomor dokumen yang baru tersimpan", got.scheme, got.err)
	}
	f.expect(t, org, invoice, now, dated(now, "INV/{YYYY}/00002"))
}

// Setiap perubahan skema masuk jejak audit, di transaksi yang sama. Yang
// dicatat adalah isian mana yang berubah, bukan isinya.
func TestChangesAreRecorded(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	first, second := uuid.New(), uuid.New()
	as := func(user uuid.UUID) context.Context {
		return testdb.With(context.Background(), testdb.Session{
			Organization: org, User: user, Permissions: []appkit.Permission{numbering.Manage},
		})
	}

	s := f.set(t, as(first), invoice, numbering.Input{Pattern: "FK/{YYYY}/{SEQ:04}", ResetPolicy: numbering.ResetYearly})
	// Simpan tanpa perubahan tidak dicatat: tidak ada yang diubah.
	s = f.set(t, as(second), invoice, numbering.Input{Pattern: "FK/{YYYY}/{SEQ:04}", ResetPolicy: numbering.ResetYearly, Version: s.Version})
	s = f.set(t, as(second), invoice, numbering.Input{Pattern: "FK/{YYYY}/{SEQ:04}", ResetPolicy: numbering.ResetNever, Version: s.Version})
	s = f.set(t, as(first), invoice, numbering.Input{Pattern: "FK/{YYYYMM}/{SEQ:04}", ResetPolicy: numbering.ResetMonthly, Version: s.Version})
	if s.Version != 3 {
		t.Errorf("version sesudah tiga perubahan = %d", s.Version)
	}
	f.set(t, as(second), receipt, numbering.Input{Pattern: "KWT{SEQ}", ResetPolicy: numbering.ResetNever})

	events := testdb.Recorded(t, f.trail, org)
	if len(events) != 4 {
		t.Fatalf("catatan = %+v", events)
	}
	for i, want := range []struct {
		actor   uuid.UUID
		target  string
		summary string
		details string
	}{
		{first, invoice, "Penomoran “Faktur” diubah.", `{"fields":["pattern"]}`},
		{second, invoice, "Penomoran “Faktur” diubah.", `{"fields":["reset_policy"]}`},
		{first, invoice, "Penomoran “Faktur” diubah.", `{"fields":["pattern","reset_policy"]}`},
		{second, receipt, "Penomoran “Kuitansi” diubah.", `{"fields":["pattern"]}`},
	} {
		e := events[i]
		details, _ := json.Marshal(e.Details)
		if e.Action != numbering.ActionSchemeUpdated || e.Category != audit.CategorySettings ||
			e.ActorID == nil || *e.ActorID != want.actor ||
			e.Target != (audit.Target{Type: numbering.TargetType, ID: want.target}) ||
			e.Summary != want.summary || string(details) != want.details {
			t.Errorf("catatan %d = %+v\nrincian %s", i, e, details)
		}
	}
	if numbering.ActionSchemeUpdated != "numbering.scheme_updated" || numbering.TargetType != "document_type" {
		t.Errorf("nama tindakan = %s, target = %s", numbering.ActionSchemeUpdated, numbering.TargetType)
	}
}

func TestRoutes(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	now := f.now()

	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", f.numbers.Routes()...)

	do := func(ctx context.Context, method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/document-numbering"},
		{http.MethodPut, "/v1/document-numbering/invoice"},
		{http.MethodPut, "/v1/document-numbering/delivery_note"},
	} {
		// Body sengaja bukan JSON: izin diperiksa sebelum body dibaca.
		if code, _ := do(staff(org), tc.method, tc.path, "bukan json"); code != http.StatusForbidden {
			t.Errorf("%s %s tanpa izin = %d, ingin 403", tc.method, tc.path, code)
		}
		if code, _ := do(context.Background(), tc.method, tc.path, "{}"); code != http.StatusUnauthorized {
			t.Errorf("%s %s tanpa sesi = %d, ingin 401", tc.method, tc.path, code)
		}
	}

	code, raw := do(admin(org), http.MethodGet, "/v1/document-numbering", "")
	if code != http.StatusOK {
		t.Fatalf("GET /document-numbering = %d, %s", code, raw)
	}
	// Bentuk JSON-nya tetap lengkap: klien tidak perlu menebak field yang hilang.
	for _, field := range []string{
		`{"data":[{"document_type":"invoice","label":"Faktur","pattern":"INV/{YYYY}/{SEQ:05}","reset_policy":"yearly","custom":false,`,
		`"next_number":"` + dated(now, "INV/{YYYY}/00001") + `","version":0,"updated_at":null}`,
		`"document_type":"sales_order"`, `"document_type":"receipt"`,
		`"tokens":[{"token":"{YYYY}","meaning":"`, `"token":"{SEQ:05}"`,
	} {
		if !strings.Contains(raw, field) {
			t.Errorf("GET /document-numbering tidak memuat %s: %s", field, raw)
		}
	}

	path := "/v1/document-numbering/invoice"
	code, raw = do(admin(org), http.MethodPut, path, `{"pattern":"FK/{YYYY}/{SEQ:04}","reset_policy":"yearly","version":0}`)
	var saved numbering.Scheme
	if err := json.Unmarshal([]byte(raw), &saved); err != nil || code != http.StatusOK ||
		saved.DocumentType != invoice || saved.Pattern != "FK/{YYYY}/{SEQ:04}" || !saved.Custom || saved.Version != 1 ||
		saved.UpdatedAt == nil || saved.NextNumber != dated(now, "FK/{YYYY}/0001") {
		t.Fatalf("PUT = %d, %s", code, raw)
	}

	// Field yang tidak dikenal ditolak, bukan diabaikan.
	code, raw = do(admin(org), http.MethodPut, path, `{"pattern":"FK/{YYYY}/{SEQ}","reset_policy":"yearly","version":1,"pola":"x"}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, `"pola"`) {
		t.Errorf("field tak dikenal = %d, %s", code, raw)
	}
	code, raw = do(admin(org), http.MethodPut, path, `{"pattern":"FK/{YYYY}","reset_policy":"harian","version":1}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, `"field":"pattern"`) || !strings.Contains(raw, `"field":"reset_policy"`) {
		t.Errorf("isian tidak sah = %d, %s", code, raw)
	}
	if code, raw := do(admin(org), http.MethodPut, path, `{"pattern":"FK/{YYYY}/{SEQ}","reset_policy":"yearly","version":"satu"}`); code != http.StatusBadRequest {
		t.Errorf("version bukan angka = %d, %s", code, raw)
	}
	if code, raw := do(admin(org), http.MethodPut, path, `{"pattern":"TIMPA/{YYYY}/{SEQ}","reset_policy":"yearly","version":0}`); code != http.StatusConflict {
		t.Errorf("PUT dengan version lama = %d, %s", code, raw)
	}
	if code, raw := do(admin(org), http.MethodPut, "/v1/document-numbering/delivery_note", `{"pattern":"SJ/{YYYY}/{SEQ}","reset_policy":"yearly","version":0}`); code != http.StatusNotFound {
		t.Errorf("PUT jenis tak dikenal = %d, %s", code, raw)
	}
	// Version skema organization ini tidak berlaku di organization lain.
	if code, raw := do(admin(uuid.New()), http.MethodPut, path, `{"pattern":"DIBAJAK/{YYYY}/{SEQ}","reset_policy":"yearly","version":1}`); code != http.StatusConflict {
		t.Errorf("PUT lintas organization = %d, %s", code, raw)
	}

	code, raw = do(admin(org), http.MethodPut, path, `{"pattern":"FK/{YYYYMM}/{SEQ:04}","reset_policy":"monthly","version":1}`)
	if code != http.StatusOK || !strings.Contains(raw, `"version":2`) || !strings.Contains(raw, `"reset_policy":"monthly"`) {
		t.Errorf("PUT kedua = %d, %s", code, raw)
	}
	code, raw = do(admin(org), http.MethodGet, "/v1/document-numbering", "")
	if code != http.StatusOK || !strings.Contains(raw, `"pattern":"FK/{YYYYMM}/{SEQ:04}","reset_policy":"monthly","custom":true`) {
		t.Errorf("GET sesudah simpan = %d, %s", code, raw)
	}

	if actions := testdb.Actions(t, f.trail, org); !slices.Equal(actions, []string{numbering.ActionSchemeUpdated, numbering.ActionSchemeUpdated}) {
		t.Errorf("tindakan tercatat = %v", actions)
	}
}

// Susunan yang tidak sah gagal saat start, bukan saat dokumen pertama dibuat.
func TestNewRejectsInvalidSetup(t *testing.T) {
	pool := testdb.New(t)
	trail := testdb.Trail(t, pool)
	f := &fixture{}

	if _, err := numbering.New(nil, trail, testdb.Hooks(), f.options()); err == nil {
		t.Error("New tanpa pool lolos")
	}
	if _, err := numbering.New(pool, nil, testdb.Hooks(), f.options()); err == nil {
		t.Error("New tanpa jejak audit lolos")
	}
	if _, err := numbering.New(pool, trail, appkit.Hooks{}, f.options()); err == nil {
		t.Error("New tanpa pengait lolos")
	}

	for name, change := range map[string]func(*numbering.Options){
		"tanpa jenis dokumen":    func(o *numbering.Options) { o.Types = nil },
		"key kosong":             func(o *numbering.Options) { o.Types[1].Key = "" },
		"key berhuruf besar":     func(o *numbering.Options) { o.Types[1].Key = "SalesOrder" },
		"key bertanda hubung":    func(o *numbering.Options) { o.Types[1].Key = "sales-order" },
		"key berawalan angka":    func(o *numbering.Options) { o.Types[1].Key = "1order" },
		"key terlalu panjang":    func(o *numbering.Options) { o.Types[1].Key = strings.Repeat("a", 41) },
		"key kembar":             func(o *numbering.Options) { o.Types[1].Key = invoice },
		"tanpa label":            func(o *numbering.Options) { o.Types[1].Label = " " },
		"label terlalu panjang":  func(o *numbering.Options) { o.Types[1].Label = strings.Repeat("a", 61) },
		"pola kosong":            func(o *numbering.Options) { o.Types[2].Pattern = "" },
		"pola tanpa nomor urut":  func(o *numbering.Options) { o.Types[2].Pattern = "KW" },
		"pola token tak dikenal": func(o *numbering.Options) { o.Types[2].Pattern = "KW/{DD}/{SEQ}" },
		"pola terlalu panjang":   func(o *numbering.Options) { o.Types[2].Pattern = strings.Repeat("A", 56) + "{SEQ}" },
		"pola lebar tidak sah":   func(o *numbering.Options) { o.Types[2].Pattern = "KW{SEQ:20}" },
		"kebijakan kosong":       func(o *numbering.Options) { o.Types[2].Reset = "" },
		"kebijakan tak dikenal":  func(o *numbering.Options) { o.Types[2].Reset = "weekly" },
		"kebijakan huruf besar":  func(o *numbering.Options) { o.Types[2].Reset = "NEVER" },
		"tahunan tanpa tahun":    func(o *numbering.Options) { o.Types[2].Reset = numbering.ResetYearly },
		"bulanan tanpa bulan":    func(o *numbering.Options) { o.Types[0].Reset = numbering.ResetMonthly },
		"zona bawaan tak dikenal": func(o *numbering.Options) {
			o.DefaultTimezone = "Mars/Olympus_Mons"
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := f.options()
			change(&opts)
			_, err := numbering.New(pool, trail, testdb.Hooks(), opts)
			if err == nil {
				t.Fatal("susunan yang tidak sah lolos")
			}
			if !strings.HasPrefix(err.Error(), "numbering: ") {
				t.Errorf("galat tanpa awalan package: %v", err)
			}
		})
	}

	// Batasnya sendiri sah: Key 40 karakter, Label 60 karakter, tanpa Timezone.
	opts := f.options()
	opts.Timezone = nil
	opts.Types = append(opts.Types, numbering.Type{
		Key: "a" + strings.Repeat("9", 39), Label: strings.Repeat("a", 60), Pattern: " {YY}{MM}-{SEQ:12} ", Reset: numbering.ResetMonthly,
	})
	if _, err := numbering.New(pool, trail, testdb.Hooks(), opts); err != nil {
		t.Errorf("susunan di batas ditolak: %v", err)
	}
}
