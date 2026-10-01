// Package media menyimpan berkas PUBLIK milik sebuah organization: logo dan
// gambar yang tampil di halaman tanpa sesi.
//
// Yang perlu diketahui sebelum memakainya:
//
//   - setiap berkas di sini dapat dibaca siapa pun yang mengetahui URL-nya
//     (`/media/{id}`). Jangan simpan dokumen pelanggan, faktur, atau berkas
//     lain yang butuh izin di sini;
//   - isi berkas tidak pernah berubah di bawah satu id. Mengganti gambar
//     berarti berkas baru dengan id baru — karena itu peramban boleh
//     menyimpannya selamanya;
//   - yang diterima hanya PNG, JPEG, dan WebP, dikenali dari ISINYA, bukan
//     dari nama berkas atau header yang dikirim klien. SVG ditolak: ia
//     dokumen yang dapat membawa skrip.
//
// Modul lain memakai Service langsung (Save, Delete); package ini sengaja
// tidak punya endpoint unggah umum. Siapa yang boleh mengunggah apa adalah
// keputusan modul pemilik berkasnya.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // decoder untuk membaca ukuran gambar
	_ "image/png"  // decoder untuk membaca ukuran gambar
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

const (
	// DefaultMaxBytes adalah batas ukuran bawaan satu berkas.
	DefaultMaxBytes = 2 << 20
	// maxDimension membatasi lebar dan tinggi gambar yang ukurannya terbaca.
	maxDimension = 8192
	// PublicPath adalah awalan URL publik berkas, relatif terhadap akar situs.
	PublicPath = "/media/"
)

// Options mengatur Service. Nilai kosong memakai bawaan.
type Options struct {
	// Store adalah penyimpanan isi berkas. Kosong: NewDBStore(pool).
	Store Store
	// MaxBytes adalah batas ukuran satu berkas. Kosong: DefaultMaxBytes.
	MaxBytes int64
}

// Service mengelola berkas media.
type Service struct {
	pool       *pgxpool.Pool
	store      Store
	maxBytes   int64
	writeError func(w http.ResponseWriter, r *http.Request, err error)
}

// New mengembalikan service media. Dari hooks hanya WriteError yang dipakai:
// satu-satunya endpoint modul ini publik, tanpa sesi.
func New(pool *pgxpool.Pool, hooks appkit.Hooks, opts Options) (*Service, error) {
	if pool == nil {
		return nil, errors.New("media: pool wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	if opts.Store == nil {
		opts.Store = NewDBStore(pool)
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	return &Service{pool: pool, store: opts.Store, maxBytes: opts.MaxBytes, writeError: hooks.WriteError}, nil
}

// File adalah satu berkas media di API.
type File struct {
	ID uuid.UUID `json:"id"`
	// URL relatif terhadap akar situs, mis. "/media/<id>". Tidak memuat
	// alamat aplikasi: alamat itu dapat berganti.
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// Width dan Height nol bila ukuran gambar tidak terbaca (WebP).
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	CreatedAt time.Time `json:"created_at"`

	organizationID uuid.UUID
	sha256         string
}

// URL mengembalikan alamat publik berkas id, relatif terhadap akar situs.
func URL(id uuid.UUID) string { return PublicPath + id.String() }

// MaxBytes adalah batas ukuran satu berkas di service ini. Modul pemanggil
// memakainya untuk membatasi body sebelum membacanya.
func (s *Service) MaxBytes() int64 { return s.maxBytes }

var errNotFound = appkit.NotFound("Berkas tidak ditemukan.")

func key(org, id uuid.UUID) string { return org.String() + "/" + id.String() }

// Save memeriksa lalu menyimpan satu gambar milik org. Pemanggil yang
// memutuskan siapa yang boleh mengunggah; Save tidak memeriksa izin.
func (s *Service) Save(ctx context.Context, org uuid.UUID, r io.Reader) (File, error) {
	if org == uuid.Nil {
		return File{}, errors.New("media: organization kosong")
	}
	// Dibaca satu byte lebih dari batas: begitu terlampaui, berhenti membaca.
	data, err := io.ReadAll(io.LimitReader(r, s.maxBytes+1))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return File{}, s.tooLarge()
		}
		return File{}, fmt.Errorf("media: membaca berkas: %w", err)
	}
	switch {
	case len(data) == 0:
		return File{}, appkit.Validation("Berkas kosong.")
	case int64(len(data)) > s.maxBytes:
		return File{}, s.tooLarge()
	}

	f := File{ID: uuid.New(), Size: int64(len(data)), organizationID: org}
	// Jenis dikenali dari isinya; header Content-Type milik klien tidak dipakai.
	f.ContentType = http.DetectContentType(data)
	switch f.ContentType {
	case "image/png", "image/jpeg":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return File{}, appkit.Validation("Berkas gambar rusak atau tidak dapat dibaca.")
		}
		if cfg.Width > maxDimension || cfg.Height > maxDimension {
			return File{}, appkit.Validation(fmt.Sprintf("Ukuran gambar maksimal %d × %d piksel.", maxDimension, maxDimension))
		}
		f.Width, f.Height = cfg.Width, cfg.Height
	case "image/webp":
	default:
		return File{}, appkit.Validation("Berkas harus berupa gambar PNG, JPEG, atau WebP.")
	}
	sum := sha256.Sum256(data)
	f.sha256 = hex.EncodeToString(sum[:])
	f.URL = URL(f.ID)

	// Isi dulu, baru barisnya: baris tanpa isi adalah URL publik yang gagal
	// dibuka, sedangkan isi tanpa baris hanya sampah yang tidak terlihat.
	k := key(org, f.ID)
	if err := s.store.Put(ctx, k, bytes.NewReader(data), f.Size, f.ContentType); err != nil {
		return File{}, fmt.Errorf("media: menyimpan isi berkas: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO appkit_media (id, organization_id, content_type, size_bytes, sha256, width, height)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0), NULLIF($7, 0))
		RETURNING created_at`,
		f.ID, org, f.ContentType, f.Size, f.sha256, f.Width, f.Height).Scan(&f.CreatedAt)
	if err != nil {
		_ = s.store.Delete(context.WithoutCancel(ctx), k)
		return File{}, fmt.Errorf("media: mencatat berkas: %w", err)
	}
	return f, nil
}

func (s *Service) tooLarge() error {
	return appkit.Validation(fmt.Sprintf("Ukuran berkas maksimal %s.", humanSize(s.maxBytes)))
}

func humanSize(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MB", n>>20)
	}
	return fmt.Sprintf("%d KB", n>>10)
}

const selectFile = `
	SELECT id, organization_id, content_type, size_bytes, sha256,
	       COALESCE(width, 0), COALESCE(height, 0), created_at
	FROM appkit_media`

func scanFile(row pgx.Row) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.organizationID, &f.ContentType, &f.Size, &f.sha256, &f.Width, &f.Height, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, errNotFound
	}
	if err != nil {
		return File{}, err
	}
	f.URL = URL(f.ID)
	return f, nil
}

// Get membaca data berkas id milik org. Berkas milik organization lain
// dijawab "tidak ditemukan".
func (s *Service) Get(ctx context.Context, org, id uuid.UUID) (File, error) {
	return scanFile(s.pool.QueryRow(ctx, selectFile+` WHERE organization_id = $1 AND id = $2`, org, id))
}

// Open membuka berkas id TANPA menyaring organization: ini jalur baca
// publik, dan id acak itulah alamatnya. Jangan dipakai untuk keputusan yang
// bergantung pada pemilik berkas — pakai Get.
func (s *Service) Open(ctx context.Context, id uuid.UUID) (File, io.ReadCloser, error) {
	f, err := scanFile(s.pool.QueryRow(ctx, selectFile+` WHERE id = $1`, id))
	if err != nil {
		return File{}, nil, err
	}
	rc, err := s.store.Open(ctx, key(f.organizationID, f.ID))
	if err != nil {
		return File{}, nil, fmt.Errorf("media: membuka isi berkas %s: %w", f.ID, err)
	}
	return f, rc, nil
}

// Delete menghapus berkas id milik org. Berkas yang sudah tidak ada bukan
// galat. Berkas yang masih dirujuk tabel lain ditolak database; lepaskan
// rujukannya lebih dulu.
func (s *Service) Delete(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM appkit_media WHERE organization_id = $1 AND id = $2`, org, id)
	if err != nil {
		return fmt.Errorf("media: menghapus berkas: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := s.store.Delete(ctx, key(org, id)); err != nil {
		return fmt.Errorf("media: menghapus isi berkas: %w", err)
	}
	return nil
}
