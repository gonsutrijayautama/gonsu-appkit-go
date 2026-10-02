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
// Isi berkas disimpan di database (bawaan) atau di object storage (package
// s3store); total pemakaian satu organization dapat dibatasi lewat
// Options.Quota.
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
	"strings"
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
	// Unlimited adalah jawaban Options.Quota untuk organization tanpa batas
	// penyimpanan.
	Unlimited int64 = -1
)

// Options mengatur Service. Nilai kosong memakai bawaan.
type Options struct {
	// Store adalah penyimpanan isi berkas. Kosong: NewDBStore(pool).
	//
	// Penyimpanan lain (mis. s3store) boleh dipasang kapan saja: berkas yang
	// isinya sudah telanjur di database tetap terbaca dari sana.
	Store Store
	// MaxBytes adalah batas ukuran SATU berkas: pengaman teknis, bukan batas
	// yang dijual. Kosong: DefaultMaxBytes.
	MaxBytes int64
	// Quota mengembalikan batas TOTAL penyimpanan org dalam byte — biasanya
	// nilai hak pakai paketnya. Kosong: tanpa batas untuk semua.
	//
	// Unlimited (atau nilai negatif apa pun) berarti tanpa batas. NOL berarti
	// tidak boleh menyimpan sama sekali, BUKAN tanpa batas: hak pakai yang
	// tidak dibawa paket dijawab nol, dan itu tidak boleh membuka penyimpanan.
	//
	// Batasnya lunak: dua unggahan yang tiba bersamaan dapat sama-sama lolos
	// dan melewatinya sebesar satu berkas.
	Quota func(ctx context.Context, org uuid.UUID) (limit int64, err error)
}

// Service mengelola berkas media.
type Service struct {
	pool       *pgxpool.Pool
	store      Store
	maxBytes   int64
	quota      func(ctx context.Context, org uuid.UUID) (int64, error)
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
	store := opts.Store
	switch store.(type) {
	case nil:
		store = NewDBStore(pool)
	case *DBStore:
	default:
		// Produk yang berpindah dari database ke penyimpanan lain tidak
		// kehilangan berkas lamanya.
		store = withFallback{primary: store, fallback: NewDBStore(pool)}
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	return &Service{pool: pool, store: store, maxBytes: opts.MaxBytes, quota: opts.Quota, writeError: hooks.WriteError}, nil
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
//
// Bila kuota org tidak cukup, galatnya berjenis appkit.KindQuotaExceeded.
func (s *Service) Save(ctx context.Context, org uuid.UUID, r io.Reader) (File, error) {
	return s.save(ctx, org, r, nil)
}

// SaveReplacing sama dengan Save, untuk berkas yang MENGGANTIKAN berkas
// replaces (nil: tidak ada yang digantikan). Ukuran berkas lama itu tidak
// dihitung ke kuota, supaya organization yang kuotanya penuh tetap dapat
// mengganti gambarnya dengan yang tidak lebih besar.
//
// SaveReplacing tidak menghapus berkas lama; pemanggil menghapusnya setelah
// berkas baru terpasang.
func (s *Service) SaveReplacing(ctx context.Context, org uuid.UUID, r io.Reader, replaces *uuid.UUID) (File, error) {
	return s.save(ctx, org, r, replaces)
}

func (s *Service) save(ctx context.Context, org uuid.UUID, r io.Reader, replaces *uuid.UUID) (File, error) {
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
	if err := s.checkQuota(ctx, org, f.Size, replaces); err != nil {
		return File{}, err
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

// Usage adalah pemakaian penyimpanan sebuah organization.
type Usage struct {
	// Bytes adalah jumlah ukuran seluruh berkasnya.
	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`
}

// Usage menjumlahkan pemakaian org dari data berkasnya. Jumlahnya sama apa
// pun penyimpanannya, karena data tentang berkas selalu di tabel.
func (s *Service) Usage(ctx context.Context, org uuid.UUID) (Usage, error) {
	var u Usage
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(size_bytes), 0)::bigint, COUNT(*)::int
		FROM appkit_media WHERE organization_id = $1`, org).Scan(&u.Bytes, &u.Files)
	if err != nil {
		return Usage{}, fmt.Errorf("media: menghitung pemakaian: %w", err)
	}
	return u, nil
}

// checkQuota menolak berkas berukuran size bila kuota org tidak cukup.
// Berkas replaces akan dihapus pemanggil, jadi ukurannya tidak dihitung.
func (s *Service) checkQuota(ctx context.Context, org uuid.UUID, size int64, replaces *uuid.UUID) error {
	if s.quota == nil {
		return nil
	}
	limit, err := s.quota(ctx, org)
	if err != nil {
		return fmt.Errorf("media: membaca kuota: %w", err)
	}
	switch {
	case limit < 0:
		return nil
	case limit == 0:
		return appkit.QuotaExceeded("Paket Anda tidak menyertakan penyimpanan berkas.")
	}
	// used adalah yang ditampilkan ke pengguna; kept yang dibandingkan.
	var used, kept int64
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(size_bytes), 0)::bigint,
		       COALESCE(SUM(size_bytes) FILTER (WHERE id IS DISTINCT FROM $2), 0)::bigint
		FROM appkit_media WHERE organization_id = $1`, org, replaces).Scan(&used, &kept)
	if err != nil {
		return fmt.Errorf("media: menghitung pemakaian: %w", err)
	}
	if kept+size > limit {
		return appkit.QuotaExceeded(fmt.Sprintf(
			"Penyimpanan penuh: %s dari %s sudah terpakai. Hapus berkas yang tidak dipakai, atau naikkan paket.",
			humanSize(used), humanSize(limit)))
	}
	return nil
}

func (s *Service) tooLarge() error {
	return appkit.Validation(fmt.Sprintf("Ukuran berkas maksimal %s.", humanSize(s.maxBytes)))
}

// humanSize menulis ukuran dengan satuan yang wajar dibaca: bilangan bulat
// bila pas, satu angka desimal bila tidak.
func humanSize(n int64) string {
	units := []struct {
		size int64
		name string
	}{{1 << 30, "GB"}, {1 << 20, "MB"}, {1 << 10, "KB"}}
	for _, u := range units {
		if n < u.size {
			continue
		}
		if n%u.size == 0 {
			return fmt.Sprintf("%d %s", n/u.size, u.name)
		}
		return strings.Replace(fmt.Sprintf("%.1f %s", float64(n)/float64(u.size), u.name), ".", ",", 1)
	}
	return fmt.Sprintf("%d byte", n)
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
