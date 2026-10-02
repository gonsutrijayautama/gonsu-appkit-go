// Package website mengatur halaman depan publik sebuah organization: web
// perusahaan singkat di alamat aplikasinya, atau hanya pintu masuk.
//
// Identitas — nama, logo, kontak, alamat — tidak diisi di sini, melainkan
// dibaca dari profil bisnis (package businessprofile). Yang diatur di sini
// adalah yang khas halaman depan: tagline, cerita, layanan, jam kerja, kanal,
// dan bagaimana tautannya tampil saat dibagikan.
//
// Dua sisi:
//
//   - Settings, lewat API bersesi: dibaca setiap pengguna, diubah pemegang
//     izin Manage;
//   - Public, TANPA sesi: gabungan profil bisnis dan pengaturan ini yang sudah
//     disaring untuk pengunjung. NPWP dan nama legal tidak pernah ada di
//     dalamnya.
//
// Halaman depan adalah probe platform, jadi jalur publiknya tidak pernah
// membuat halaman gagal: RenderHome mengembalikan halaman apa adanya bila
// datanya tidak dapat dibaca.
package website

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/businessprofile"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
)

// Manage adalah izin mengubah pengaturan website dan gambarnya.
const Manage appkit.Permission = "settings.website.manage"

// Mode halaman depan.
const (
	// ModeSignIn: hanya pintu masuk aplikasi. Bawaan, dan pilihan organization
	// yang tidak ingin punya web publik: isi lain tidak disajikan sama sekali.
	ModeSignIn = "signin"
	// ModeSite: web perusahaan — tentang, layanan, kontak — beserta pintu masuk.
	ModeSite = "site"
)

// DefaultCacheTTL adalah umur tampilan publik di memori.
const DefaultCacheTTL = 15 * time.Second

// Options mengatur Service.
type Options struct {
	// PublicOrganization menentukan organization sebuah permintaan TANPA sesi
	// (halaman depan, /site.json). Wajib. Produk satu-organization
	// mengembalikan organization pemasangannya; yang melayani banyak
	// organization menurunkannya dari alamat permintaan.
	PublicOrganization func(r *http.Request) (uuid.UUID, error)
	// CacheTTL: berapa lama tampilan publik disimpan di memori. Perubahan
	// profil bisnis tampil di halaman depan paling lambat sesudah selang ini.
	// Kosong: DefaultCacheTTL.
	CacheTTL time.Duration
	// Logger mencatat kegagalan jalur publik, yang sengaja tidak ditampilkan
	// ke pengunjung. Boleh kosong.
	Logger *slog.Logger
}

// Service mengelola pengaturan website.
type Service struct {
	pool     *pgxpool.Pool
	profiles *businessprofile.Service
	media    *media.Service
	hooks    appkit.Hooks
	opts     Options

	mu    sync.Mutex
	cache map[uuid.UUID]cached
}

type cached struct {
	site Public
	at   time.Time
}

// New mengembalikan service website. Identitas dibaca lewat profiles, dan
// gambar disimpan lewat m.
func New(pool *pgxpool.Pool, profiles *businessprofile.Service, m *media.Service, hooks appkit.Hooks, opts Options) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("website: pool wajib diisi")
	case profiles == nil:
		return nil, errors.New("website: service profil bisnis wajib diisi")
	case m == nil:
		return nil, errors.New("website: service media wajib diisi")
	case opts.PublicOrganization == nil:
		return nil, errors.New("website: Options.PublicOrganization wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{pool: pool, profiles: profiles, media: m, hooks: hooks, opts: opts, cache: map[uuid.UUID]cached{}}, nil
}

// Settings adalah pengaturan website di API bersesi.
type Settings struct {
	Document
	// Icons adalah nama ikon yang boleh dipakai sebuah layanan.
	Icons []string `json:"icons"`
	// Version dikirim balik saat menyimpan (Input.Version): simpan yang
	// membawa version lama ditolak. 0 bila belum pernah disimpan.
	Version int `json:"version"`
	// UpdatedAt null bila belum ada yang pernah disimpan.
	UpdatedAt *time.Time `json:"updated_at"`
}

// Document adalah isi pengaturan. Isian yang belum diisi berupa string
// kosong, bukan null.
type Document struct {
	// Mode: ModeSignIn atau ModeSite.
	Mode    string `json:"mode"`
	Tagline string `json:"tagline"`
	// Summary: satu-dua kalimat tentang bisnisnya, di atas daftar layanan.
	Summary  string   `json:"summary"`
	About    About    `json:"about"`
	Services []Item   `json:"services"`
	Contact  Contact  `json:"contact"`
	Channels Channels `json:"channels"`
	SEO      SEO      `json:"seo"`
}

// About adalah bagian "Tentang kami".
type About struct {
	Text string `json:"text"`
	// Image diatur lewat PUT /website/images/about, bukan lewat simpan.
	Image *media.File `json:"image"`
}

// Item adalah satu layanan.
type Item struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// Icon adalah salah satu dari Icons.
	Icon string `json:"icon"`
}

// Contact melengkapi kontak dari profil bisnis.
type Contact struct {
	// Hours: hari dan jam kerja, teks bebas.
	Hours string `json:"hours"`
	// MapURL: tautan ke layanan peta. Peta tidak disematkan: menyematkannya
	// mengirim alamat IP setiap pengunjung ke pihak ketiga.
	MapURL string `json:"map_url"`
	// HideAddress menyembunyikan alamat jalan dari halaman publik — usaha
	// perorangan sering beralamat rumah. Kota tetap tampil.
	HideAddress bool `json:"hide_address"`
}

// Channels adalah kanal lain bisnisnya. WhatsApp berupa nomor (digit, dengan
// kode negara); sisanya alamat https lengkap.
type Channels struct {
	WhatsApp  string `json:"whatsapp"`
	Instagram string `json:"instagram"`
	Facebook  string `json:"facebook"`
	TikTok    string `json:"tiktok"`
	YouTube   string `json:"youtube"`
	LinkedIn  string `json:"linkedin"`
}

// SEO mengatur judul dan pratinjau saat tautan halaman depan dibagikan.
// Yang kosong diturunkan dari nama bisnis, tagline, dan ringkasan.
type SEO struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// Image diatur lewat PUT /website/images/seo. Kosong: logo bisnis.
	Image *media.File `json:"image"`
}

// documentSchema adalah nomor bentuk dokumen yang disimpan. Isian baru yang
// hanya menambah tidak menaikkannya.
const documentSchema = 1

// stored adalah dokumen seperti tersimpan di kolom settings: hanya teks.
// Gambar tidak pernah masuk dokumen; ia kolom tersendiri.
type stored struct {
	Schema         int      `json:"schema"`
	Mode           string   `json:"mode"`
	Tagline        string   `json:"tagline"`
	Summary        string   `json:"summary"`
	AboutText      string   `json:"about_text"`
	Services       []Item   `json:"services"`
	Contact        Contact  `json:"contact"`
	Channels       Channels `json:"channels"`
	SEOTitle       string   `json:"seo_title"`
	SEODescription string   `json:"seo_description"`
}

func (d stored) document() Document {
	doc := Document{
		Mode: d.Mode, Tagline: d.Tagline, Summary: d.Summary,
		About: About{Text: d.AboutText}, Services: d.Services,
		Contact: d.Contact, Channels: d.Channels,
		SEO: SEO{Title: d.SEOTitle, Description: d.SEODescription},
	}
	fill(&doc)
	return doc
}

var errChanged = appkit.Conflict("Pengaturan website sudah diubah orang lain. Muat ulang halaman, lalu ulangi perubahan Anda.")

// Get membaca pengaturan organization request ini. Setiap pengguna yang sudah
// login boleh membacanya. Pengaturan yang belum pernah disimpan dikembalikan
// dengan bawaannya (ModeSignIn), bukan galat.
func (s *Service) Get(ctx context.Context) (Settings, error) {
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Settings{}, err
	}
	return s.lookup(ctx, org)
}

func (s *Service) lookup(ctx context.Context, org uuid.UUID) (Settings, error) {
	var (
		raw        []byte
		about, seo *uuid.UUID
		at         time.Time
		out        = Settings{Icons: Icons}
	)
	err := s.pool.QueryRow(ctx, `
		SELECT settings, about_media_id, seo_media_id, version, updated_at
		FROM appkit_websites
		WHERE organization_id = $1`, org).Scan(&raw, &about, &seo, &out.Version, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Document = defaults()
		return out, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("website: membaca pengaturan: %w", err)
	}
	var doc stored
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Settings{}, fmt.Errorf("website: dokumen pengaturan rusak: %w", err)
	}
	out.Document = doc.document()
	out.UpdatedAt = &at
	for _, image := range []struct {
		id     *uuid.UUID
		target **media.File
	}{{about, &out.About.Image}, {seo, &out.SEO.Image}} {
		if image.id == nil {
			continue
		}
		f, err := s.media.Get(ctx, org, *image.id)
		if err != nil {
			return Settings{}, fmt.Errorf("website: membaca gambar: %w", err)
		}
		*image.target = &f
	}
	return out, nil
}

func defaults() Document {
	d := Document{}
	fill(&d)
	return d
}

// fill mengisi yang kosong dengan bawaannya, supaya bentuk JSON-nya tetap:
// mode selalu terisi dan services selalu berupa daftar.
func fill(d *Document) {
	if d.Mode == "" {
		d.Mode = ModeSignIn
	}
	if d.Services == nil {
		d.Services = []Item{}
	}
}

// Update menyimpan seluruh pengaturan (bukan sebagian). Gambar tidak ikut
// berubah.
//
// in.Version harus sama dengan Version pengaturan yang dibaca; bila sudah
// berubah sejak itu, simpan ditolak dengan galat KindConflict.
func (s *Service) Update(ctx context.Context, in Input) (Settings, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return Settings{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Settings{}, err
	}
	if errs := in.normalize(); len(errs) > 0 {
		return Settings{}, appkit.Validation("Isian belum lengkap.", errs...)
	}
	raw, err := json.Marshal(in.stored())
	if err != nil {
		return Settings{}, err
	}
	// Sama dengan profil bisnis: baris baru hanya untuk version 0; baris yang
	// ada hanya berubah bila version-nya masih yang dibaca pengirim.
	var version int
	err = s.pool.QueryRow(ctx, `
		INSERT INTO appkit_websites (organization_id, settings, version)
		SELECT $1, $2, 1 WHERE $3 = 0
		ON CONFLICT (organization_id) DO UPDATE SET
			settings = EXCLUDED.settings, version = appkit_websites.version + 1, updated_at = now()
		WHERE appkit_websites.version = 0
		RETURNING version`, org, raw, in.Version).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) && in.Version > 0 {
		err = s.pool.QueryRow(ctx, `
			UPDATE appkit_websites SET settings = $2, version = version + 1, updated_at = now()
			WHERE organization_id = $1 AND version = $3
			RETURNING version`, org, raw, in.Version).Scan(&version)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, errChanged
	}
	if err != nil {
		return Settings{}, fmt.Errorf("website: menyimpan pengaturan: %w", err)
	}
	s.forget(org)
	return s.lookup(ctx, org)
}

// Slot adalah tempat gambar di pengaturan website.
type Slot string

const (
	// SlotAbout: foto di bagian "Tentang kami".
	SlotAbout Slot = "about"
	// SlotSEO: gambar pratinjau saat tautan dibagikan.
	SlotSEO Slot = "seo"
)

// column adalah kolom yang menyimpan gambar slot ini. Nilainya dari daftar
// tetap di bawah, tidak pernah dari request.
func (slot Slot) column() (string, bool) {
	switch slot {
	case SlotAbout:
		return "about_media_id", true
	case SlotSEO:
		return "seo_media_id", true
	}
	return "", false
}

var errNoSlot = appkit.NotFound("Tempat gambar tidak dikenal.")

// SetImage mengganti gambar slot dengan gambar dari r. Gambar lama dihapus.
func (s *Service) SetImage(ctx context.Context, slot Slot, r io.Reader) (Settings, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return Settings{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Settings{}, err
	}
	if _, ok := slot.column(); !ok {
		return Settings{}, errNoSlot
	}
	f, err := s.media.Save(ctx, org, r)
	if err != nil {
		return Settings{}, err
	}
	if err := s.swapImage(ctx, org, slot, &f.ID); err != nil {
		// Berkas baru belum dirujuk siapa pun; jangan ditinggal menjadi sampah.
		_ = s.media.Delete(context.WithoutCancel(ctx), org, f.ID)
		return Settings{}, err
	}
	return s.lookup(ctx, org)
}

// RemoveImage menghapus gambar slot. Slot tanpa gambar bukan galat.
func (s *Service) RemoveImage(ctx context.Context, slot Slot) (Settings, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return Settings{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Settings{}, err
	}
	if _, ok := slot.column(); !ok {
		return Settings{}, errNoSlot
	}
	if err := s.swapImage(ctx, org, slot, nil); err != nil {
		return Settings{}, err
	}
	return s.lookup(ctx, org)
}

// swapImage memasang gambar baru (nil: tanpa gambar) lalu menghapus berkas
// lama. Baris pengaturan dikunci selama pertukaran, supaya dua unggahan
// bersamaan tidak meninggalkan berkas yang tidak dirujuk siapa pun.
func (s *Service) swapImage(ctx context.Context, org uuid.UUID, slot Slot, image *uuid.UUID) error {
	column, _ := slot.column()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Gambar boleh diunggah sebelum pengaturan pertama kali disimpan.
	if _, err := tx.Exec(ctx, `
		INSERT INTO appkit_websites (organization_id) VALUES ($1)
		ON CONFLICT (organization_id) DO NOTHING`, org); err != nil {
		return fmt.Errorf("website: menyiapkan pengaturan: %w", err)
	}
	var old *uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT `+column+` FROM appkit_websites
		WHERE organization_id = $1 FOR UPDATE`, org).Scan(&old); err != nil {
		return fmt.Errorf("website: mengunci pengaturan: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE appkit_websites SET `+column+` = $2, updated_at = now()
		WHERE organization_id = $1`, org, image); err != nil {
		return fmt.Errorf("website: memasang gambar: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.forget(org)
	if old != nil {
		// Gambar sudah berganti; gagal menghapus berkas lama tidak membatalkannya.
		_ = s.media.Delete(context.WithoutCancel(ctx), org, *old)
	}
	return nil
}
