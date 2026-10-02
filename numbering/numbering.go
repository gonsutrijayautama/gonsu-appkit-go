// Package numbering membagikan nomor dokumen: nomor order, faktur, surat
// jalan, dan dokumen lain milik produk.
//
// Pembagian tugasnya:
//
//   - JENIS DOKUMEN ada di kode. Produk mendaftarkannya (Options.Types)
//     beserta pola dan kebijakan reset bawaannya. Menambah jenis dokumen tidak
//     menuntut migrasi;
//   - BENTUK NOMOR milik organization. Administratornya mengganti pola dan
//     kebijakan reset dari layar pengaturan (SetScheme). Baris skema hanya ada
//     bila bawaannya diganti, jadi organization baru tidak perlu disiapkan;
//   - NOMOR URUT dibagikan Next, DI DALAM transaksi dokumennya.
//
// Yang dijamin Next, juga saat dua dokumen disimpan bersamaan:
//
//   - tanpa nomor kembar: dua dokumen tidak pernah mendapat nomor urut yang
//     sama;
//   - tanpa nomor lompat: nomor urut naik di transaksi dokumennya, jadi dokumen
//     yang batal disimpan mengembalikan nomornya, dan dokumen berikutnya
//     memakainya.
//
// Harganya, yang diterima dengan sengaja: dua pengguna yang membuat jenis
// dokumen yang sama di organization yang sama bergiliran. Yang kedua menunggu
// sampai transaksi yang pertama selesai — commit atau batal. Jenis dokumen
// lain dan organization lain tidak ikut menunggu. Karena itu transaksi dokumen
// harus singkat: tidak memanggil layanan luar selagi memegang nomor.
//
// Nomor urut tidak pernah dihitung dari tabel dokumen (SELECT MAX(...)+1):
// dua transaksi bersamaan membaca nilai yang sama.
package numbering

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	// Image rilis tidak membawa tzdata; zona waktu organization dibutuhkan
	// untuk tahun dan bulan pada nomor, dan untuk lingkup reset-nya.
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
)

// Manage adalah izin melihat dan mengubah skema penomoran dokumen.
const Manage appkit.Permission = "settings.numbering.manage"

// Kebijakan reset: kapan nomor urut kembali ke 1.
const (
	// ResetNever: tidak pernah; nomor urut terus naik.
	ResetNever = "never"
	// ResetYearly: setiap pergantian tahun.
	ResetYearly = "yearly"
	// ResetMonthly: setiap pergantian bulan.
	ResetMonthly = "monthly"
)

// DefaultTimezone dipakai bila Options.DefaultTimezone kosong.
const DefaultTimezone = "Asia/Jakarta"

// Type adalah satu jenis dokumen yang dinomori.
type Type struct {
	// Key adalah yang diserahkan produk ke Next, dan yang ada di URL layar
	// pengaturan: huruf kecil, angka, dan garis bawah. Tidak diubah setelah
	// dirilis: skema dan nomor urut tersimpan menurut Key.
	Key string
	// Label tampil di layar pengaturan dan di jejak audit, mis. "Faktur".
	// Wajib, maksimal 60 karakter.
	Label string
	// Pattern dan Reset adalah bawaannya: berlaku di setiap organization yang
	// belum menggantinya. Keduanya diperiksa New dengan aturan yang sama
	// dengan layar pengaturan.
	//
	// Mengganti Reset bawaan jenis yang sudah dirilis memindahkan setiap
	// organization yang memakai bawaan ke nomor urut lingkup lain, tanpa
	// melanjutkan yang sedang berjalan (lihat SetScheme). Hindari.
	Pattern string
	Reset   string
}

// Options mengatur Service. Types wajib.
type Options struct {
	// Types adalah jenis dokumen produk, dalam urutan tampilnya.
	Types []Type
	// Timezone mengembalikan zona waktu org, mis. "Asia/Makassar". Tahun dan
	// bulan pada nomor — dan lingkup reset-nya — mengikuti waktu setempat
	// organization, bukan UTC: dokumen yang dibuat 1 Januari pukul 05.00 WIB
	// tidak boleh mendapat nomor bertahun lalu.
	//
	// Boleh kosong: seluruh organization memakai DefaultTimezone. Jawaban
	// kosong atau zona yang tidak dikenal juga jatuh ke DefaultTimezone.
	//
	// Next memanggilnya selagi transaksi dokumen terbuka, jadi jawabannya
	// sebaiknya cepat — dari memori atau cache, bukan query per panggilan.
	Timezone func(ctx context.Context, org uuid.UUID) string
	// DefaultTimezone kosong: "Asia/Jakarta".
	DefaultTimezone string
}

// Service membagikan nomor dokumen dan mengelola skemanya.
type Service struct {
	pool  *pgxpool.Pool
	trail *audit.Service
	hooks appkit.Hooks
	opts  Options

	// fallback adalah Options.DefaultTimezone yang sudah dimuat.
	fallback *time.Location
	// types menurut urutan Options.Types; byKey indeksnya.
	types []Type
	byKey map[string]int
}

var reKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// maxLabel menjaga ringkasan jejak audit, yang menyebut Label, tetap di bawah
// batas panjangnya.
const maxLabel = 60

// New mengembalikan service penomoran. Perubahan skema dicatat lewat trail.
// Jenis dokumen dan bawaannya diperiksa di sini: susunan yang tidak sah gagal
// saat start, bukan saat dokumen pertama dibuat.
func New(pool *pgxpool.Pool, trail *audit.Service, hooks appkit.Hooks, opts Options) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("numbering: pool wajib diisi")
	case trail == nil:
		return nil, errors.New("numbering: service jejak audit wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	if opts.DefaultTimezone == "" {
		opts.DefaultTimezone = DefaultTimezone
	}
	fallback, err := time.LoadLocation(opts.DefaultTimezone)
	if err != nil {
		return nil, fmt.Errorf("numbering: Options.DefaultTimezone %q tidak dikenal: %w", opts.DefaultTimezone, err)
	}
	s := &Service{pool: pool, trail: trail, hooks: hooks, opts: opts, fallback: fallback}
	if err := s.loadTypes(opts.Types); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) loadTypes(types []Type) error {
	if len(types) == 0 {
		return errors.New("numbering: Options.Types wajib diisi")
	}
	s.byKey = make(map[string]int, len(types))
	for _, t := range types {
		t.Label, t.Pattern, t.Reset = strings.TrimSpace(t.Label), strings.TrimSpace(t.Pattern), strings.TrimSpace(t.Reset)
		switch {
		case !reKey.MatchString(t.Key):
			return fmt.Errorf("numbering: Key jenis dokumen %q tidak sah", t.Key)
		case t.Label == "":
			return fmt.Errorf("numbering: jenis dokumen %s tanpa Label", t.Key)
		case utf8.RuneCountInString(t.Label) > maxLabel:
			return fmt.Errorf("numbering: Label jenis dokumen %s lebih dari %d karakter", t.Key, maxLabel)
		}
		if _, dup := s.byKey[t.Key]; dup {
			return fmt.Errorf("numbering: jenis dokumen %s terdaftar dua kali", t.Key)
		}
		// Bawaan yang ditolak layar pengaturan tidak boleh berlaku lewat kode.
		if errs := check(t.Pattern, t.Reset); len(errs) > 0 {
			return fmt.Errorf("numbering: bawaan jenis dokumen %s tidak sah (%s): %s", t.Key, errs[0].Field, errs[0].Message)
		}
		s.byKey[t.Key] = len(s.types)
		s.types = append(s.types, t)
	}
	return nil
}

// location mengembalikan zona waktu org. Zona yang tidak dikenal tidak
// menggagalkan pembuatan dokumen: nomor yang tahunnya meleset masih jauh
// lebih baik daripada dokumen yang tidak tersimpan.
func (s *Service) location(ctx context.Context, org uuid.UUID) *time.Location {
	if s.opts.Timezone == nil {
		return s.fallback
	}
	name := s.opts.Timezone(ctx, org)
	if name == "" {
		return s.fallback
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return s.fallback
	}
	return loc
}

// scopeOf menentukan lingkup nomor urut pada waktu setempat t: nomor urut
// kembali ke 1 di lingkup baru.
func scopeOf(reset string, t time.Time) string {
	switch reset {
	case ResetYearly:
		return t.Format("2006")
	case ResetMonthly:
		return t.Format("2006-01")
	}
	return "-"
}

func validReset(reset string) bool {
	return reset == ResetNever || reset == ResetYearly || reset == ResetMonthly
}

// querier dipenuhi *pgxpool.Pool dan pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// effective adalah skema yang BERLAKU untuk satu jenis dokumen di sebuah
// organization: baris appkit_number_schemes-nya, atau bawaan dari kode.
type effective struct {
	pattern string
	reset   string
	// version 0 dan updatedAt nil: tidak ada baris; bawaan yang berlaku.
	version   int
	updatedAt *time.Time
}

func (e effective) custom() bool { return e.version > 0 }

func (t Type) defaults() effective { return effective{pattern: t.Pattern, reset: t.Reset} }

// read membaca skema yang berlaku untuk t di org.
func (s *Service) read(ctx context.Context, db querier, org uuid.UUID, t Type) (effective, error) {
	var (
		e  effective
		at time.Time
	)
	err := db.QueryRow(ctx, `
		SELECT pattern, reset_policy, version, updated_at
		FROM appkit_number_schemes
		WHERE organization_id = $1 AND document_type = $2`, org, t.Key).
		Scan(&e.pattern, &e.reset, &e.version, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return t.defaults(), nil
	}
	if err != nil {
		return effective{}, fmt.Errorf("numbering: membaca skema %s: %w", t.Key, err)
	}
	e.updatedAt = &at
	return e, nil
}

// lock mengambil kunci penomoran (org, docType) sampai transaksi tx selesai.
// Next dan SetScheme sama-sama mengambilnya, sehingga skema tidak pernah
// berganti di antara saat Next membacanya dan saat nomor urutnya naik.
//
// Kuncinya tingkat TRANSAKSI: kunci tingkat sesi tidak menjaga apa pun di
// belakang PgBouncer `pool_mode=transaction`.
func lock(ctx context.Context, tx pgx.Tx, org uuid.UUID, docType string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"appkit_numbering:"+org.String()+":"+docType); err != nil {
		return fmt.Errorf("numbering: mengunci penomoran %s: %w", docType, err)
	}
	return nil
}

// Next mengalokasikan nomor berikutnya untuk dokumen berjenis docType milik
// org, pada waktu dokumen at.
//
// WAJIB dipanggil di dalam transaksi dokumennya (tx), yaitu transaksi yang
// juga menyimpan dokumen itu. Nomor urut naik di tx: bila tx batal, nomornya
// kembali dan dipakai dokumen berikutnya, sehingga tidak ada nomor lompat.
// Memanggilnya di transaksi tersendiri membuang jaminan itu.
//
// Harganya: sejak Next menjawab sampai tx selesai, pemanggil Next lain untuk
// jenis dokumen yang sama di organization yang sama menunggu. Itu diterima
// dengan sengaja; lihat komentar package. Transaksi yang mengambil nomor
// beberapa jenis dokumen mengambilnya dalam urutan yang sama di setiap jalur,
// supaya dua transaksi tidak saling menunggu.
//
// org diserahkan modul pemanggil, yang membacanya dari Hooks.Organization
// untuk request itu — tidak pernah dari body, query, atau environment. Next
// tidak membaca sesi dan tidak memeriksa izin: izin membuat dokumen sudah
// diperiksa pemanggilnya.
//
// Tahun dan bulan pada nomor, dan lingkup reset-nya, dihitung dari at menurut
// zona waktu org (Options.Timezone).
//
// docType yang tidak terdaftar di Options.Types adalah galat pemrogram, bukan
// galat isian pengguna.
//
// tx memakai tingkat isolasi bawaan PostgreSQL (READ COMMITTED). Di tingkat
// yang lebih ketat, penyimpanan bersamaan dijawab database dengan galat
// serialisasi yang harus diulang pemanggil.
func (s *Service) Next(ctx context.Context, tx pgx.Tx, org uuid.UUID, docType string, at time.Time) (string, error) {
	i, ok := s.byKey[docType]
	switch {
	case !ok:
		return "", fmt.Errorf("numbering: jenis dokumen %q tidak terdaftar di Options.Types", docType)
	case tx == nil:
		return "", errors.New("numbering: Next wajib dipanggil di dalam transaksi dokumennya")
	case org == uuid.Nil:
		return "", errors.New("numbering: organization kosong")
	case at.IsZero():
		return "", errors.New("numbering: waktu dokumen kosong")
	}
	local := at.In(s.location(ctx, org))

	if err := lock(ctx, tx, org, docType); err != nil {
		return "", err
	}
	e, err := s.read(ctx, tx, org, s.types[i])
	if err != nil {
		return "", err
	}
	// Pola diurai SEBELUM nomor urut naik: pola tersimpan yang rusak tidak
	// boleh memakai nomor.
	l, problem := compile(e.pattern)
	if problem != "" {
		return "", fmt.Errorf("numbering: pola tersimpan %s %q tidak sah: %s", docType, e.pattern, problem)
	}

	// Satu pernyataan yang atomik, dan baris counter-nya terkunci sampai tx
	// selesai: itulah yang membuat nomor tidak kembar dan tidak lompat.
	var seq int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO appkit_number_counters (organization_id, document_type, scope, last_value)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (organization_id, document_type, scope)
		DO UPDATE SET last_value = appkit_number_counters.last_value + 1
		RETURNING last_value`, org, docType, scopeOf(e.reset, local)).Scan(&seq); err != nil {
		return "", fmt.Errorf("numbering: mengalokasikan nomor %s: %w", docType, err)
	}
	return l.render(local, seq), nil
}

// Token pola.
const (
	tokenYear      = "YYYY"
	tokenShortYear = "YY"
	tokenYearMonth = "YYYYMM"
	tokenMonth     = "MM"
	tokenSeq       = "SEQ"
)

// Batas isi pola. maxPattern sama dengan constraint kolomnya.
const (
	maxPattern  = 60
	maxSeqWidth = 12
)

// Token adalah satu token pola beserta artinya, untuk bantuan di layar
// pengaturan.
type Token struct {
	Token   string `json:"token"`
	Meaning string `json:"meaning"`
}

// Tokens mengembalikan token yang dikenal pola. Daftarnya dikirim ke layar
// supaya bantuannya tidak disalin ke frontend dan menyimpang saat token
// bertambah.
func Tokens() []Token {
	return []Token{
		{"{YYYY}", "Tahun empat angka, mis. 2026."},
		{"{YY}", "Tahun dua angka, mis. 26."},
		{"{YYYYMM}", "Tahun dan bulan, mis. 202609."},
		{"{MM}", "Bulan dua angka, mis. 09."},
		{"{SEQ}", "Nomor urut, mis. 7."},
		{"{SEQ:05}", fmt.Sprintf("Nomor urut dengan nol di depan, mis. 00007. Lebarnya 1 sampai %d angka.", maxSeqWidth)},
	}
}

// segment adalah satu potong pola: teks apa adanya, atau satu token.
type segment struct {
	// token kosong: text ditulis apa adanya.
	token string
	text  string
	// width adalah lebar {SEQ:NN}; 0 berarti tanpa nol di depan.
	width int
}

// layout adalah pola yang sudah diurai.
type layout []segment

func (l layout) has(tokens ...string) bool {
	for _, seg := range l {
		for _, token := range tokens {
			if seg.token == token {
				return true
			}
		}
	}
	return false
}

// render menyusun nomor dari l untuk waktu setempat t dan nomor urut seq.
func (l layout) render(t time.Time, seq int64) string {
	var b strings.Builder
	for _, seg := range l {
		switch seg.token {
		case tokenYear:
			b.WriteString(t.Format("2006"))
		case tokenShortYear:
			b.WriteString(t.Format("06"))
		case tokenYearMonth:
			b.WriteString(t.Format("200601"))
		case tokenMonth:
			b.WriteString(t.Format("01"))
		case tokenSeq:
			fmt.Fprintf(&b, "%0*d", seg.width, seq)
		default:
			b.WriteString(seg.text)
		}
	}
	return b.String()
}

var reSeqWidth = regexp.MustCompile(`^SEQ:([0-9]{1,2})$`)

// Pesan pola, untuk ditampilkan di isiannya.
const (
	problemBraces  = "Kurung kurawal hanya untuk token, mis. {SEQ}, dan harus berpasangan."
	problemUnknown = "Token {%s} tidak dikenal. Yang tersedia: {YYYY}, {YY}, {YYYYMM}, {MM}, {SEQ}, dan {SEQ:05}."
	problemWidth   = "Lebar nomor urut ditulis 1 sampai %d, mis. {SEQ:05}."
)

// compile mengurai pattern dan memeriksa yang tidak bergantung pada kebijakan
// reset. problem adalah pesan untuk isian pola; kosong bila polanya sah.
func compile(pattern string) (l layout, problem string) {
	switch {
	case pattern == "":
		return nil, "Pola wajib diisi."
	case utf8.RuneCountInString(pattern) > maxPattern:
		return nil, fmt.Sprintf("Pola maksimal %d karakter.", maxPattern)
	case strings.ContainsFunc(pattern, unicode.IsControl):
		// Nomor dicetak dan dicari: baris baru atau tab di dalamnya tidak pernah
		// disengaja.
		return nil, "Pola tidak boleh memuat baris baru atau karakter kendali."
	}
	for rest := pattern; rest != ""; {
		open := strings.IndexAny(rest, "{}")
		if open < 0 {
			l = append(l, segment{text: rest})
			break
		}
		if rest[open] == '}' {
			return nil, problemBraces
		}
		if open > 0 {
			l = append(l, segment{text: rest[:open]})
		}
		rest = rest[open+1:]
		end := strings.IndexAny(rest, "{}")
		if end < 0 || rest[end] == '{' {
			return nil, problemBraces
		}
		name := rest[:end]
		rest = rest[end+1:]

		switch name {
		case tokenYear, tokenShortYear, tokenYearMonth, tokenMonth, tokenSeq:
			l = append(l, segment{token: name})
			continue
		}
		digits, isSeq := strings.CutPrefix(name, tokenSeq+":")
		if !isSeq {
			return nil, fmt.Sprintf(problemUnknown, name)
		}
		width := 0
		if reSeqWidth.MatchString(name) {
			width, _ = strconv.Atoi(digits)
		}
		if width < 1 || width > maxSeqWidth {
			return nil, fmt.Sprintf(problemWidth, maxSeqWidth)
		}
		l = append(l, segment{token: tokenSeq, width: width})
	}
	if !l.has(tokenSeq) {
		return nil, "Pola wajib memuat {SEQ}: tanpa nomor urut, setiap dokumen mendapat nomor yang sama."
	}
	return l, ""
}

// check memeriksa pola bersama kebijakan resetnya, dan mengembalikan galat per
// isian (`pattern`, `reset_policy`). Dipakai layar pengaturan (SetScheme) dan
// New, sehingga bawaan dari kode tunduk pada aturan yang sama.
//
// Pola wajib memuat masa yang menjadi lingkup reset-nya. Nomor urut yang
// kembali ke 1 setiap tahun tanpa tahun di polanya menghasilkan nomor yang
// SAMA PERSIS dengan dokumen pertama tahun lalu: urutannya mulai lagi,
// sedangkan nomor yang tercetak tidak berubah, dan dua dokumen mendapat nomor
// yang sama. Begitu pula reset bulanan tanpa tahun dan bulan.
func check(pattern, reset string) []appkit.FieldError {
	var errs []appkit.FieldError
	fail := func(field, message string) {
		errs = append(errs, appkit.FieldError{Field: field, Message: message})
	}

	l, problem := compile(pattern)
	if problem != "" {
		fail("pattern", problem)
	}
	if !validReset(reset) {
		fail("reset_policy", "Pilih kapan nomor kembali ke 1: tidak pernah, setiap tahun, atau setiap bulan.")
		return errs
	}
	if problem != "" {
		return errs
	}

	year := l.has(tokenYear, tokenShortYear, tokenYearMonth)
	month := l.has(tokenYearMonth) || (year && l.has(tokenMonth))
	switch {
	case reset == ResetYearly && !year:
		fail("pattern", "Nomor yang kembali ke 1 setiap tahun wajib memuat tahun: {YYYY}, {YY}, atau {YYYYMM}. Tanpanya, nomor tahun ini sama dengan nomor tahun lalu.")
	case reset == ResetMonthly && !month:
		fail("pattern", "Nomor yang kembali ke 1 setiap bulan wajib memuat tahun dan bulan: {YYYYMM}, atau {YYYY} bersama {MM}. Tanpanya, nomor bulan ini sama dengan nomor bulan lalu.")
	}
	return errs
}

// Format menyusun nomor dari pattern, mis. "INV/{YYYY}/{SEQ:05}", untuk waktu
// t dan nomor urut seq. Tahun dan bulan dibaca dari t apa adanya: pemanggil
// yang menginginkan waktu setempat menyerahkan t di zona itu.
//
// Token yang dikenal ada di Tokens. Pola yang ditolak layar pengaturan —
// tanpa {SEQ}, memuat token yang tidak dikenal, atau terlalu panjang — juga
// ditolak di sini. Syarat yang bergantung pada kebijakan reset tidak
// diperiksa: Format tidak mengenal kebijakannya.
func Format(pattern string, t time.Time, seq int64) (string, error) {
	l, problem := compile(pattern)
	if problem != "" {
		return "", fmt.Errorf("numbering: pola %q tidak sah: %s", pattern, problem)
	}
	if seq < 1 {
		return "", fmt.Errorf("numbering: nomor urut %d tidak sah", seq)
	}
	return l.render(t, seq), nil
}
