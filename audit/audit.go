// Package audit menyimpan jejak audit sebuah organization: siapa melakukan
// apa, kapan, dan dari alamat mana.
//
// Yang dicatat, dalam empat kelompok:
//
//   - CategoryAccess: siapa boleh masuk dan boleh apa — akses diberikan atau
//     dicabut, role pengguna diubah, role dibuat atau diubah izinnya;
//   - CategorySettings: pengaturan organization — profil bisnis, website;
//   - CategorySession: masuk, keluar, dan sesi yang diputus;
//   - CategoryActivity: tindakan penting milik produk, yang dicatat produk
//     sendiri — menyetujui dokumen, menghapus, mengubah harga.
//
// Yang TIDAK dicatat: pembacaan data, dan isi yang rahasia. Untuk sebuah
// perubahan cukup apa yang diubah; Details tidak pernah memuat sandi, token,
// atau isi dokumen.
//
// Catatan hanya dapat DITAMBAH. Package ini tidak punya jalur mengubah atau
// menghapusnya, dan database menolak UPDATE atas tabelnya. Berapa lama
// catatan disimpan belum ditetapkan, jadi tidak ada penghapusan otomatis.
//
// Modul library mencatat sendiri perubahannya, di transaksi yang sama dengan
// perubahannya (RecordTx): perubahan tanpa catatan tidak pernah tersimpan.
// Produk mencatat miliknya lewat Record, RecordTx, RecordFor, atau
// RecordForTx.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// View adalah izin membaca jejak audit.
const View appkit.Permission = "settings.audit.view"

// Category mengelompokkan catatan di layar riwayat.
type Category string

const (
	CategoryAccess   Category = "access"
	CategorySettings Category = "settings"
	CategorySession  Category = "session"
	CategoryActivity Category = "activity"
)

func (c Category) valid() bool {
	switch c {
	case CategoryAccess, CategorySettings, CategorySession, CategoryActivity:
		return true
	}
	return false
}

// Target adalah yang dikenai sebuah tindakan. Kosong bila tidak ada.
type Target struct {
	// Type: jenisnya, mis. "role" atau "user".
	Type string `json:"type"`
	// ID: pengenalnya di dalam produk. Tetap tercatat setelah yang ditunjuknya
	// dihapus.
	ID string `json:"id"`
}

// Entry adalah satu tindakan yang akan dicatat.
type Entry struct {
	Category Category
	// Action menamai tindakannya: huruf kecil bertitik, `<benda>.<kejadian>`,
	// mis. "role.created". Nama yang sudah dipakai tidak diubah maknanya:
	// catatan lama tetap menyebutnya.
	Action string
	Target Target
	// Summary adalah satu kalimat untuk layar riwayat, mis. "Role “Kasir”
	// dibuat." Wajib.
	Summary string
	// Details adalah rinciannya: apa yang diubah. Boleh kosong. Jangan
	// memasukkan isi yang rahasia.
	Details map[string]any
}

// Batas isi, sama dengan constraint kolomnya.
const (
	maxAction     = 100
	maxTargetType = 60
	maxTargetID   = 200
	maxSummary    = 300
	maxActorName  = 200
	// maxDetails membatasi Details sesudah dijadikan JSON: catatan adalah
	// ringkasan perubahan, bukan salinan datanya.
	maxDetails = 16 << 10
)

var reAction = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// prepare merapikan e dan mengembalikan Details sebagai JSON. Galatnya galat
// pemrogram, bukan galat isian pengguna.
func (e *Entry) prepare() ([]byte, error) {
	e.Summary = strings.TrimSpace(e.Summary)
	switch {
	case !e.Category.valid():
		return nil, fmt.Errorf("audit: kelompok %q tidak dikenal", e.Category)
	case len(e.Action) > maxAction || !reAction.MatchString(e.Action):
		return nil, fmt.Errorf("audit: nama tindakan %q tidak sah", e.Action)
	case e.Summary == "" || utf8.RuneCountInString(e.Summary) > maxSummary:
		return nil, fmt.Errorf("audit: Summary tindakan %s wajib diisi, maksimal %d karakter", e.Action, maxSummary)
	case utf8.RuneCountInString(e.Target.Type) > maxTargetType || utf8.RuneCountInString(e.Target.ID) > maxTargetID:
		return nil, fmt.Errorf("audit: Target tindakan %s terlalu panjang", e.Action)
	}
	if len(e.Details) == 0 {
		return []byte(`{}`), nil
	}
	raw, err := json.Marshal(e.Details)
	if err != nil {
		return nil, fmt.Errorf("audit: Details tindakan %s: %w", e.Action, err)
	}
	if len(raw) > maxDetails {
		return nil, fmt.Errorf("audit: Details tindakan %s melebihi %d byte", e.Action, maxDetails)
	}
	return raw, nil
}

// Options mengatur Service. Nilai kosong memakai bawaan.
type Options struct {
	// ClientAddr mengembalikan alamat asal request ini. Hanya produk yang
	// mengetahuinya: di belakang proxy, alamat koneksi bukan alamat
	// penggunanya. Kosong, atau alamat yang tidak sah: catatan tanpa alamat.
	ClientAddr func(ctx context.Context) netip.Addr
	// ActorName mengembalikan nama tampil pengguna actor di org, untuk
	// dicatat bersama id-nya — biasanya users.Service.ActorName, dirangkai
	// dengan penutup karena users sendiri mencatat lewat service ini. Nama
	// yang dicatat adalah nama SAAT ITU. Kosong, atau jawaban kosong: catatan
	// tanpa nama.
	//
	// Dipanggil selagi transaksi pemanggil terbuka, jadi jawablah cepat, dan
	// jangan menggagalkan: nama yang tidak terbaca dijawab string kosong.
	ActorName func(ctx context.Context, org, actor uuid.UUID) string
}

// Service mencatat dan membaca jejak audit.
type Service struct {
	pool  *pgxpool.Pool
	hooks appkit.Hooks
	opts  Options
}

// New mengembalikan service jejak audit. Hooks.User wajib: catatan menyebut
// pelakunya.
func New(pool *pgxpool.Pool, hooks appkit.Hooks, opts Options) (*Service, error) {
	if pool == nil {
		return nil, errors.New("audit: pool wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	if hooks.User == nil {
		return nil, errors.New("audit: Hooks.User wajib diisi")
	}
	return &Service{pool: pool, hooks: hooks, opts: opts}, nil
}

// execer dipenuhi *pgxpool.Pool dan pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Record mencatat tindakan pengguna request ini di organization-nya. Record
// tidak memeriksa izin: yang dicatat adalah tindakan yang sudah diizinkan
// pemanggilnya.
//
// Catatannya berdiri sendiri. Untuk perubahan yang tidak boleh tersimpan
// tanpa catatannya, pakai RecordTx.
func (s *Service) Record(ctx context.Context, e Entry) error {
	return s.record(ctx, s.pool, e)
}

// RecordTx sama dengan Record, di dalam transaksi tx milik pemanggil:
// catatan tersimpan hanya bila perubahannya tersimpan, dan sebaliknya.
func (s *Service) RecordTx(ctx context.Context, tx pgx.Tx, e Entry) error {
	return s.record(ctx, tx, e)
}

func (s *Service) record(ctx context.Context, db execer, e Entry) error {
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return err
	}
	actor, err := s.hooks.User(ctx)
	if err != nil {
		return err
	}
	return s.insert(ctx, db, org, &actor, e)
}

// RecordFor mencatat tindakan di org TANPA sesi: untuk kejadian yang terjadi
// sebelum ada sesi atau di luar permintaan pengguna — berhasil masuk, sesi
// yang diputus karena aksesnya dicabut, perintah operator. org ditentukan
// kode server, tidak pernah dari body atau query.
//
// actor adalah pengguna yang melakukannya; uuid.Nil berarti bukan tindakan
// seorang pengguna.
func (s *Service) RecordFor(ctx context.Context, org, actor uuid.UUID, e Entry) error {
	return s.recordFor(ctx, s.pool, org, actor, e)
}

// RecordForTx sama dengan RecordFor, di dalam transaksi tx milik pemanggil.
func (s *Service) RecordForTx(ctx context.Context, tx pgx.Tx, org, actor uuid.UUID, e Entry) error {
	return s.recordFor(ctx, tx, org, actor, e)
}

func (s *Service) recordFor(ctx context.Context, db execer, org, actor uuid.UUID, e Entry) error {
	if org == uuid.Nil {
		return errors.New("audit: organization kosong")
	}
	if actor == uuid.Nil {
		return s.insert(ctx, db, org, nil, e)
	}
	return s.insert(ctx, db, org, &actor, e)
}

func (s *Service) insert(ctx context.Context, db execer, org uuid.UUID, actor *uuid.UUID, e Entry) error {
	details, err := e.prepare()
	if err != nil {
		return err
	}
	var addr *string
	if s.opts.ClientAddr != nil {
		// Zona antarmuka tidak bermakna di luar mesinnya, dan alamat IPv4 yang
		// dibungkus IPv6 dicatat sebagai IPv4.
		if a := s.opts.ClientAddr(ctx).Unmap().WithZone(""); a.IsValid() {
			text := a.String()
			addr = &text
		}
	}
	var name string
	if actor != nil && s.opts.ActorName != nil {
		name = strings.TrimSpace(s.opts.ActorName(ctx, org, *actor))
		if utf8.RuneCountInString(name) > maxActorName {
			name = string([]rune(name)[:maxActorName])
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO appkit_audit_events (id, organization_id, category, action, actor_id, actor_name,
			target_type, target_id, summary, details, client_addr)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::inet)`,
		id, org, string(e.Category), e.Action, actor, name,
		e.Target.Type, e.Target.ID, e.Summary, details, addr); err != nil {
		return fmt.Errorf("audit: mencatat %s: %w", e.Action, err)
	}
	return nil
}

// Event adalah satu catatan di API.
type Event struct {
	ID       uuid.UUID `json:"id"`
	Category Category  `json:"category"`
	Action   string    `json:"action"`
	// ActorID adalah id pengguna di dalam produk (Hooks.User); null bila bukan
	// tindakan seorang pengguna. Nama pelakunya dicari produk dari id ini.
	ActorID *uuid.UUID `json:"actor_id"`
	// ActorName adalah nama pelaku saat tindakan dilakukan (Options.ActorName);
	// kosong bila tidak diketahui.
	ActorName string         `json:"actor_name"`
	Target    Target         `json:"target"`
	Summary   string         `json:"summary"`
	Details   map[string]any `json:"details"`
	// ClientAddr null bila alamat asalnya tidak diketahui.
	ClientAddr *string   `json:"client_addr"`
	CreatedAt  time.Time `json:"created_at"`
}

// Query menyaring List.
type Query struct {
	// Category kosong: semua kelompok.
	Category Category
	// Before adalah Page.Next halaman sebelumnya; uuid.Nil untuk halaman
	// pertama.
	Before uuid.UUID
	// Limit di bawah satu memakai bawaan (50); paling banyak 200.
	Limit int
}

// Page adalah satu halaman catatan, terbaru dulu.
type Page struct {
	Data []Event `json:"data"`
	// Next dikirim balik sebagai Query.Before untuk halaman berikutnya; null
	// bila ini halaman terakhir.
	Next *uuid.UUID `json:"next"`
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

// List mengembalikan catatan organization request ini, terbaru dulu.
func (s *Service) List(ctx context.Context, q Query) (Page, error) {
	if err := s.hooks.Authorize(ctx, View); err != nil {
		return Page{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Page{}, err
	}
	if q.Category != "" && !q.Category.valid() {
		return Page{}, appkit.Validation("Saringan belum sesuai.",
			appkit.FieldError{Field: "category", Message: "Kelompok tidak dikenal."})
	}
	if q.Limit < 1 {
		q.Limit = defaultLimit
	}
	q.Limit = min(q.Limit, maxLimit)
	var before *uuid.UUID
	if q.Before != uuid.Nil {
		before = &q.Before
	}
	// Satu baris lebih dari yang diminta, untuk mengetahui ada-tidaknya halaman
	// berikutnya. Penanda halaman dicari di organization yang sama: penanda
	// milik organization lain menghasilkan halaman kosong.
	rows, err := s.pool.Query(ctx, `
		SELECT id, category, action, actor_id, actor_name, target_type, target_id, summary, details,
		       host(client_addr), created_at
		FROM appkit_audit_events
		WHERE organization_id = $1
		  AND ($2 = '' OR category = $2)
		  AND ($3::uuid IS NULL OR (created_at, id) < (
		        SELECT created_at, id FROM appkit_audit_events
		        WHERE organization_id = $1 AND id = $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, org, string(q.Category), before, q.Limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("audit: membaca catatan: %w", err)
	}
	defer rows.Close()
	page := Page{Data: []Event{}}
	for rows.Next() {
		var (
			e        Event
			category string
			details  []byte
		)
		if err := rows.Scan(&e.ID, &category, &e.Action, &e.ActorID, &e.ActorName, &e.Target.Type, &e.Target.ID,
			&e.Summary, &details, &e.ClientAddr, &e.CreatedAt); err != nil {
			return Page{}, fmt.Errorf("audit: membaca catatan: %w", err)
		}
		e.Category = Category(category)
		if err := json.Unmarshal(details, &e.Details); err != nil {
			return Page{}, fmt.Errorf("audit: rincian catatan %s rusak: %w", e.ID, err)
		}
		page.Data = append(page.Data, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("audit: membaca catatan: %w", err)
	}
	if len(page.Data) > q.Limit {
		page.Data = page.Data[:q.Limit]
		page.Next = &page.Data[q.Limit-1].ID
	}
	return page, nil
}
