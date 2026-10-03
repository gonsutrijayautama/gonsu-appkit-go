// Package pages menyimpan halaman publik sebuah organization yang disusun
// dengan penyusun halaman: beranda, layanan, tentang, dan seterusnya.
//
// Pembagian tugasnya:
//
//   - penyusun halaman (editor) dan blok-bloknya ada di frontend produk. Isi
//     halaman adalah datanya, berupa JSON; package ini tidak menggambar apa
//     pun;
//   - JENIS BLOK didaftarkan produk di kode (Options.Blocks), beserta jenis
//     tiap isiannya. Server memeriksa setiap isi yang disimpan terhadap
//     daftar itu: blok yang tidak dikenal ditolak, teks berformat
//     dibersihkan, tautan javascript: dan gambar dari luar ditolak. Isi yang
//     kelak disusun AI melewati pemeriksaan yang sama;
//   - yang disusun selalu DRAF. Pengunjung hanya melihat isi yang
//     diterbitkan, dan setiap terbitan disimpan di riwayat (beberapa terakhir
//     per halaman) untuk dikembalikan ke draf;
//   - identitas — nama, logo, kontak, kanal — tetap di package website, dan
//     mode-nya berlaku: di mode "signin" tidak ada halaman yang tampil, walau
//     sudah terbit;
//   - jumlah halaman mengikuti paket (Options.Limit). Paket yang tidak
//     menyertakan penyusun halaman membekukannya: halaman yang ada tetap
//     tampil, tetapi tidak dapat ditambah, diubah, diterbitkan, atau
//     dikembalikan. Batal terbit dan hapus tetap boleh.
//
// Halaman publik disajikan server produk: RenderPage menyisipkan isi halaman
// terbit ke kerangka HTML hasil build frontend, seperti website.RenderHome.
package pages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/microcosm-cc/bluemonday"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
	"github.com/gonsutrijayautama/gonsu-appkit-go/website"
)

// Manage adalah izin menyusun, menerbitkan, dan menghapus halaman.
const Manage appkit.Permission = "settings.pages.manage"

// HomePath adalah alamat beranda. Alamatnya tidak dapat diganti.
const HomePath = "/"

// Unlimited adalah jawaban Options.Limit untuk organization tanpa batas
// jumlah halaman.
const Unlimited int64 = -1

// DefaultMaxRevisions adalah banyaknya terbitan yang disimpan per halaman.
const DefaultMaxRevisions = 20

// Status halaman.
const (
	// StatusDraft: belum, atau tidak lagi, terbit.
	StatusDraft = "draft"
	// StatusPublished: terbit, dan drafnya sama dengan yang tampil.
	StatusPublished = "published"
	// StatusChanged: terbit, tetapi drafnya sudah diubah dan belum diterbitkan.
	StatusChanged = "changed"
)

// Options mengatur Service. Blocks dan Limit wajib.
type Options struct {
	// Blocks adalah jenis blok yang boleh dipakai, sama dengan konfigurasi
	// penyusun halaman di frontend produk.
	Blocks []Block
	// Root adalah isian tingkat halaman di konfigurasi penyusun halaman.
	// Kosong: halaman tidak punya isian tingkat halaman.
	Root map[string]Field
	// Limit mengembalikan jumlah halaman yang boleh dimiliki org — nilai hak
	// pakai paketnya, karena paket bertingkat menyertakan jumlah halaman yang
	// berbeda. Wajib.
	//
	// Unlimited (atau nilai negatif apa pun) berarti tanpa batas. NOL berarti
	// paketnya tidak menyertakan penyusun halaman, BUKAN tanpa batas: hak
	// pakai yang tidak dibawa paket dijawab nol. Organization yang turun ke
	// paket dengan batas lebih kecil dari jumlah halamannya tetap dapat
	// menyusun halaman yang ada; yang ditolak hanya menambah.
	Limit func(ctx context.Context, org uuid.UUID) (int64, error)
	// Reserved adalah alamat yang dipakai produk sendiri dan tidak boleh
	// dipakai halaman, mis. "/login", "/settings", "/v1", dan route kerangka
	// halaman. "/media" selalu termasuk.
	Reserved []string
	// ImportLegacy menyusun isi beranda dari isi halaman website versi lama
	// (website.Legacy). Hanya produk yang tahu nama bloknya. Kosong: tawaran
	// impor tidak ada.
	ImportLegacy func(ctx context.Context, legacy website.Legacy) (json.RawMessage, error)
	// MaxDocumentBytes membatasi isi satu halaman. Kosong:
	// DefaultMaxDocumentBytes.
	MaxDocumentBytes int
	// MaxRevisions adalah banyaknya terbitan yang disimpan per halaman.
	// Kosong: DefaultMaxRevisions.
	MaxRevisions int
	// Logger mencatat kegagalan yang sengaja tidak ditampilkan: halaman publik
	// dan pembersihan gambar. Boleh kosong.
	Logger *slog.Logger
}

// Service mengelola halaman.
type Service struct {
	pool     *pgxpool.Pool
	sites    *website.Service
	files    *media.Service
	trail    *audit.Service
	hooks    appkit.Hooks
	opts     Options
	blocks   map[string]Block
	policy   *bluemonday.Policy
	reserved map[string]bool
}

// New mengembalikan service halaman. Mode dan identitas dibaca lewat sites,
// gambar disimpan lewat files, dan perubahan dicatat lewat trail. Katalog
// blok diperiksa di sini: susunan yang salah gagal saat start.
func New(pool *pgxpool.Pool, sites *website.Service, files *media.Service, trail *audit.Service, hooks appkit.Hooks, opts Options) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("pages: pool wajib diisi")
	case sites == nil:
		return nil, errors.New("pages: service website wajib diisi")
	case files == nil:
		return nil, errors.New("pages: service media wajib diisi")
	case trail == nil:
		return nil, errors.New("pages: service jejak audit wajib diisi")
	case opts.Limit == nil:
		return nil, errors.New("pages: Options.Limit wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	if hooks.User == nil {
		return nil, errors.New("pages: Hooks.User wajib diisi")
	}
	blocks, err := checkCatalog(opts.Blocks, opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.MaxDocumentBytes <= 0 {
		opts.MaxDocumentBytes = DefaultMaxDocumentBytes
	}
	if opts.MaxRevisions <= 0 {
		opts.MaxRevisions = DefaultMaxRevisions
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	reserved := map[string]bool{"/media": true}
	for _, path := range opts.Reserved {
		first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if first == "" {
			return nil, fmt.Errorf("pages: alamat terlarang %q tidak sah", path)
		}
		reserved["/"+strings.ToLower(first)] = true
	}
	return &Service{
		pool: pool, sites: sites, files: files, trail: trail, hooks: hooks, opts: opts,
		blocks: blocks, policy: richText(), reserved: reserved,
	}, nil
}

// Navigation menyebut apakah halaman tampil di menu situs, dan urutannya.
type Navigation struct {
	Visible  bool `json:"visible"`
	Position int  `json:"position"`
}

// SEO adalah judul dan pratinjau tautan sebuah halaman. Yang kosong
// diturunkan dari judul halaman dan identitas website.
type SEO struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// Image diatur lewat PUT /pages/{id}/images/seo.
	Image *media.File `json:"image"`
}

// Page adalah satu halaman di API bersesi.
type Page struct {
	ID         uuid.UUID  `json:"id"`
	Path       string     `json:"path"`
	Title      string     `json:"title"`
	Navigation Navigation `json:"navigation"`
	SEO        SEO        `json:"seo"`
	// Status: StatusDraft, StatusPublished, atau StatusChanged.
	Status string `json:"status"`
	// Draft adalah data penyusun halaman yang sedang disusun. Hanya ada di
	// jawaban satu halaman, tidak di daftar.
	Draft json.RawMessage `json:"draft,omitempty"`
	// Version dikirim balik saat menyimpan dan menerbitkan.
	Version int `json:"version"`
	// PublishedNumber adalah nomor terbitan yang sedang tampil; null bila
	// tidak terbit.
	PublishedNumber *int       `json:"published_number"`
	PublishedAt     *time.Time `json:"published_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

const selectPage = `
	SELECT id, path, title, nav_visible, nav_position, seo_title, seo_description, seo_media_id,
	       CASE WHEN published IS NULL THEN 'draft' WHEN draft = published THEN 'published' ELSE 'changed' END,
	       version, published_number, published_at, updated_at`

var errNotFound = appkit.NotFound("Halaman tidak ditemukan.")

// scanPage membaca satu baris selectPage, ditambah kolom extra sesudahnya.
func (s *Service) scanPage(ctx context.Context, org uuid.UUID, row pgx.Row, extra ...any) (Page, error) {
	var (
		p   Page
		seo *uuid.UUID
	)
	dest := append([]any{&p.ID, &p.Path, &p.Title, &p.Navigation.Visible, &p.Navigation.Position,
		&p.SEO.Title, &p.SEO.Description, &seo, &p.Status, &p.Version, &p.PublishedNumber, &p.PublishedAt, &p.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Page{}, errNotFound
	}
	if err != nil {
		return Page{}, fmt.Errorf("pages: membaca halaman: %w", err)
	}
	if seo != nil {
		f, err := s.files.Get(ctx, org, *seo)
		if err != nil {
			return Page{}, fmt.Errorf("pages: membaca gambar pratinjau: %w", err)
		}
		p.SEO.Image = &f
	}
	return p, nil
}

// Listing adalah jawaban GET /pages.
type Listing struct {
	// Data: beranda dulu, lalu urut menu, lalu alamat. Tanpa isi draf.
	Data   []Page `json:"data"`
	Limit  Limit  `json:"limit"`
	Legacy Legacy `json:"legacy"`
}

// Limit adalah batas jumlah halaman organization ini.
type Limit struct {
	// Enabled false: paketnya tidak menyertakan penyusun halaman. Halaman yang
	// ada tetap tampil, tetapi tidak dapat ditambah atau diubah.
	Enabled bool `json:"enabled"`
	Count   int  `json:"count"`
	// Max null berarti tanpa batas.
	Max *int64 `json:"max"`
}

// Legacy menyebut apakah isi website versi lama dapat diimpor ke beranda.
type Legacy struct {
	Available bool `json:"available"`
}

// begin memeriksa izin Manage, lalu membaca organization request ini.
func (s *Service) begin(ctx context.Context) (uuid.UUID, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return uuid.Nil, err
	}
	return s.hooks.Organization(ctx)
}

func (s *Service) limit(ctx context.Context, org uuid.UUID) (int64, error) {
	n, err := s.opts.Limit(ctx, org)
	if err != nil {
		return 0, fmt.Errorf("pages: membaca batas halaman: %w", err)
	}
	return n, nil
}

var errDisabled = appkit.QuotaExceeded(appkit.LimitPages,
	"Paket Anda tidak menyertakan penyusun halaman. Halaman yang sudah terbit tetap tampil, tetapi tidak dapat diubah atau ditambah.")

// requireEnabled menolak menyusun bila paket org tidak menyertakan penyusun
// halaman.
func (s *Service) requireEnabled(ctx context.Context, org uuid.UUID) error {
	n, err := s.limit(ctx, org)
	if err != nil {
		return err
	}
	if n == 0 {
		return errDisabled
	}
	return nil
}

// List mengembalikan halaman organization request ini.
func (s *Service) List(ctx context.Context) (Listing, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Listing{}, err
	}
	rows, err := s.pool.Query(ctx, selectPage+` FROM appkit_pages WHERE organization_id = $1
		ORDER BY path <> '/', nav_position, path`, org)
	if err != nil {
		return Listing{}, fmt.Errorf("pages: membaca halaman: %w", err)
	}
	var found []Page
	for rows.Next() {
		p, err := s.scanPage(ctx, org, rows)
		if err != nil {
			rows.Close()
			return Listing{}, err
		}
		found = append(found, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Listing{}, fmt.Errorf("pages: membaca halaman: %w", err)
	}
	out := Listing{Data: slices.Concat([]Page{}, found)}
	limit, err := s.limit(ctx, org)
	if err != nil {
		return Listing{}, err
	}
	out.Limit = Limit{Enabled: limit != 0, Count: len(found)}
	if limit >= 0 {
		out.Limit.Max = &limit
	}
	if out.Legacy.Available, err = s.legacyAvailable(ctx, org); err != nil {
		return Listing{}, err
	}
	return out, nil
}

// Get membaca satu halaman organization request ini, beserta drafnya.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	return s.get(ctx, s.pool, org, id)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Service) get(ctx context.Context, db querier, org, id uuid.UUID) (Page, error) {
	var draft []byte
	p, err := s.scanPage(ctx, org, db.QueryRow(ctx, selectPage+`, draft FROM appkit_pages
		WHERE organization_id = $1 AND id = $2`, org, id), &draft)
	if err != nil {
		return Page{}, err
	}
	p.Draft = draft
	return p, nil
}
