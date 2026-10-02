// Package attachments menyimpan berkas PRIVAT milik sebuah organization:
// lampiran surat perintah kerja, pindaian kontrak, foto pemeriksaan mutu —
// berkas yang hanya boleh dilihat orang yang berhak.
//
// Yang perlu diketahui sebelum memakainya:
//
//   - package ini pasangan privat package media. Berkas yang tampil di halaman
//     tanpa sesi (logo, gambar halaman depan) tempatnya di media; berkas yang
//     butuh izin tempatnya di sini;
//   - package ini TIDAK punya route dan tidak punya jalur baca publik. Setiap
//     pembacaan dan penghapusan menyaring organization, tanpa pengecualian;
//     berkas milik organization lain dijawab "tidak ditemukan";
//   - SIAPA yang boleh membaca atau mengunggah lampiran yang mana diputuskan
//     modul produk pemilik dokumennya. Modul itu memeriksa izin atas DOKUMEN
//     itu, baru memanggil Service dengan dokumen yang sama sebagai Owner.
//     Service tidak memeriksa izin dan tidak membaca sesi: organization dan
//     pengunggah diserahkan modul pemanggil, yang membacanya dari pengaitnya
//     (appkit.Hooks) — tidak pernah dari body atau query;
//   - sebuah berkas hanya dapat dibaca dan dihapus LEWAT dokumen yang
//     dilampirinya: Get, Open, dan Delete meminta Owner, dan berkas dokumen
//     lain — walau satu organization — dijawab "tidak ditemukan". Izin atas
//     satu dokumen tidak pernah membuka lampiran dokumen lain;
//   - jenis berkas dikenali dari ISINYA, bukan dari nama berkas atau header
//     yang dikirim klien. SVG dan HTML tidak pernah diterima: keduanya dokumen
//     yang dapat membawa skrip;
//   - berkas selalu disajikan sebagai unduhan (Serve), tidak pernah
//     ditampilkan di dalam halaman.
//
// Isi berkas disimpan lewat media.Store — database (bawaan) atau object
// storage (package media/s3store) — di bawah key berawalan "private/", jadi
// tidak pernah bertabrakan dengan berkas media walau penyimpanannya sama.
// Kuota penyimpanannya biasanya hak pakai yang sama dengan media; lihat
// Options.OtherUsage.
//
// Di handler produk, setelah izin atas surat perintah kerja orderID diperiksa
// — id berkas dari URL tidak perlu diperiksa lagi, karena hanya lampiran
// dokumen itu yang dapat terbuka:
//
//	owner := attachments.Owner{Type: "work_order", ID: orderID}
//	f, rc, err := files.Open(ctx, org, owner, id)
//	if err != nil { ... }
//	defer rc.Close()
//	attachments.Serve(w, r, f, rc)
package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
)

const (
	// DefaultMaxBytes adalah batas ukuran bawaan satu berkas.
	DefaultMaxBytes = 10 << 20
	// keyPrefix memisahkan isi berkas privat dari isi berkas media
	// ("<organization>/<id>") di penyimpanan yang sama.
	keyPrefix = "private/"
	// maxFilename membatasi panjang nama berkas, dalam karakter.
	maxFilename = 200
	// fallbackFilename adalah nama berkas yang namanya kosong setelah
	// dibersihkan.
	fallbackFilename = "berkas"
)

// DefaultTypes adalah jenis berkas yang diterima bila Options.Types kosong.
var DefaultTypes = []string{"application/pdf", "image/png", "image/jpeg", "image/webp"}

// forbiddenTypes tidak pernah diterima, apa pun isi Options.Types: dokumen
// yang dapat membawa skrip.
var forbiddenTypes = []string{"image/svg+xml", "text/html", "application/xhtml+xml"}

// typeNames menyebut jenis yang lazim dengan nama yang dikenal pengguna, untuk
// pesan galat. Jenis lain disebut apa adanya.
var typeNames = map[string]string{
	"application/pdf": "PDF",
	"image/png":       "PNG",
	"image/jpeg":      "JPEG",
	"image/webp":      "WebP",
}

var (
	ownerTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,59}$`)
	// Sama dengan CHECK kolom content_type.
	contentTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$`)
)

// Options mengatur Service. Nilai kosong memakai bawaan.
type Options struct {
	// Store adalah penyimpanan isi berkas. Kosong: media.NewDBStore(pool).
	//
	// Penyimpanan lain (mis. s3store) boleh dipasang kapan saja: berkas yang
	// isinya sudah telanjur di database tetap terbaca dari sana. Store yang
	// sama boleh dipakai bersama media.Service; key keduanya tidak pernah
	// bertabrakan.
	Store media.Store
	// MaxBytes adalah batas ukuran SATU berkas: pengaman teknis, bukan batas
	// yang dijual. Kosong: DefaultMaxBytes.
	MaxBytes int64
	// Types adalah jenis berkas yang diterima, mis. "application/pdf".
	// Kosong: DefaultTypes.
	//
	// Jenis dikenali dari ISI berkas dengan http.DetectContentType, jadi yang
	// dapat lolos hanya jenis yang dikenalinya: berkas .docx dan .xlsx,
	// misalnya, dikenali sebagai "application/zip". Jenis hasil pengenalan
	// itu pula yang disimpan dan dipakai Serve.
	//
	// SVG dan HTML tidak pernah diterima; New menolak Types yang memuatnya.
	Types []string
	// Quota mengembalikan batas TOTAL penyimpanan org dalam byte — biasanya
	// nilai hak pakai paketnya. Kosong: tanpa batas untuk semua.
	//
	// Artinya sama dengan media.Options.Quota: media.Unlimited (atau nilai
	// negatif apa pun) berarti tanpa batas, sedangkan NOL berarti tidak boleh
	// menyimpan sama sekali, BUKAN tanpa batas. Batasnya lunak: dua unggahan
	// yang tiba bersamaan dapat sama-sama lolos dan melewatinya sebesar satu
	// berkas.
	Quota func(ctx context.Context, org uuid.UUID) (limit int64, err error)
	// OtherUsage mengembalikan byte yang sudah dipakai org DI LUAR package
	// ini terhadap batas yang sama. Kosong: hanya berkas package ini yang
	// dihitung.
	//
	// Kuota penyimpanan biasanya satu hak pakai untuk media dan lampiran
	// sekaligus, jadi isinya biasanya pemakaian media — dan media.Options
	// punya OtherUsage yang sama untuk arah sebaliknya. Keduanya dipasang
	// dengan closure, karena masing-masing butuh Usage milik yang lain:
	//
	//	var files *attachments.Service
	//	logos, err := media.New(pool, hooks, media.Options{
	//		Quota: quota,
	//		OtherUsage: func(ctx context.Context, org uuid.UUID) (int64, error) {
	//			u, err := files.Usage(ctx, org)
	//			return u.Bytes, err
	//		},
	//	})
	//	files, err = attachments.New(pool, attachments.Options{
	//		Quota: quota,
	//		OtherUsage: func(ctx context.Context, org uuid.UUID) (int64, error) {
	//			u, err := logos.Usage(ctx, org)
	//			return u.Bytes, err
	//		},
	//	})
	//
	// OtherUsage hanya dipanggil bila Quota memberi batas; galatnya
	// menggagalkan unggahan.
	OtherUsage func(ctx context.Context, org uuid.UUID) (bytes int64, err error)
}

// Service mengelola berkas privat.
type Service struct {
	pool       *pgxpool.Pool
	store      media.Store
	maxBytes   int64
	types      []string
	wrongType  string
	quota      func(ctx context.Context, org uuid.UUID) (int64, error)
	otherUsage func(ctx context.Context, org uuid.UUID) (int64, error)
}

// New mengembalikan service berkas privat. Tanpa pengait: package ini tidak
// punya route dan tidak pernah membaca sesi.
func New(pool *pgxpool.Pool, opts Options) (*Service, error) {
	if pool == nil {
		return nil, errors.New("attachments: pool wajib diisi")
	}
	types, err := allowedTypes(opts.Types)
	if err != nil {
		return nil, err
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = t
		if name, ok := typeNames[t]; ok {
			names[i] = name
		}
	}
	return &Service{
		pool: pool, store: media.WithDBFallback(opts.Store, pool), maxBytes: opts.MaxBytes,
		types:      types,
		wrongType:  "Jenis berkas ini tidak diterima. Yang diterima: " + strings.Join(names, ", ") + ".",
		quota:      opts.Quota,
		otherUsage: opts.OtherUsage,
	}, nil
}

// allowedTypes memeriksa dan merapikan Options.Types. Salah tulis harus gagal
// saat start, bukan menjadi unggahan yang selalu ditolak.
func allowedTypes(types []string) ([]string, error) {
	if len(types) == 0 {
		types = DefaultTypes
	}
	out := make([]string, 0, len(types))
	for _, t := range types {
		t = strings.ToLower(strings.TrimSpace(t))
		if len(t) > 100 || !contentTypePattern.MatchString(t) {
			return nil, fmt.Errorf("attachments: Types memuat %q, yang bukan jenis berkas", t)
		}
		if slices.Contains(forbiddenTypes, t) {
			return nil, fmt.Errorf("attachments: Types memuat %s; SVG dan HTML tidak pernah diterima", t)
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

// Owner menyebut dokumen yang dilampiri sebuah berkas, mis. surat perintah
// kerja beserta id-nya. Dokumen itu milik produk; package ini hanya menyimpan
// sebutannya dan tidak memeriksa keberadaannya.
type Owner struct {
	// Type adalah jenis dokumennya, mis. "work_order": huruf kecil, angka,
	// dan garis bawah, diawali huruf, paling panjang 60 karakter.
	Type string `json:"type"`
	// ID adalah id dokumen itu di dalam produk, 1–200 karakter.
	ID string `json:"id"`
}

// validate menolak Owner yang tidak sah. Galatnya bukan untuk pengguna: Owner
// diisi kode modul pemanggil, bukan isian.
func (o Owner) validate() error {
	if !ownerTypePattern.MatchString(o.Type) {
		return fmt.Errorf("attachments: Owner.Type %q tidak sah", o.Type)
	}
	n := utf8.RuneCountInString(o.ID)
	if n < 1 || n > 200 || !utf8.ValidString(o.ID) || strings.ContainsFunc(o.ID, unicode.IsControl) {
		return errors.New("attachments: Owner.ID harus 1–200 karakter, tanpa karakter kendali")
	}
	return nil
}

// File adalah satu berkas privat di API. Tidak ada URL: berkas ini tidak
// punya alamat publik, dan alamat unduhnya adalah route milik modul pemanggil.
type File struct {
	ID    uuid.UUID `json:"id"`
	Owner Owner     `json:"owner"`
	// Filename adalah nama asli berkas yang sudah dibersihkan, untuk nama
	// unduhan. Jenis berkas TIDAK dibaca dari sini.
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// UploadedBy adalah id pengguna di dalam produk; null bila berkasnya
	// bukan hasil tindakan seorang pengguna.
	UploadedBy *uuid.UUID `json:"uploaded_by"`
	CreatedAt  time.Time  `json:"created_at"`
}

// MaxBytes adalah batas ukuran satu berkas di service ini. Modul pemanggil
// memakainya untuk membatasi body sebelum membacanya.
func (s *Service) MaxBytes() int64 { return s.maxBytes }

var errNotFound = appkit.NotFound("Berkas tidak ditemukan.")

func key(org, id uuid.UUID) string { return keyPrefix + org.String() + "/" + id.String() }

// Save memeriksa lalu menyimpan satu berkas milik org sebagai lampiran owner.
// Pemanggil yang memutuskan siapa yang boleh mengunggah; Save tidak memeriksa
// izin, dan tidak memeriksa bahwa owner ada.
//
// org dan uploadedBy diisi pemanggil dari pengaitnya (Hooks.Organization dan
// Hooks.User). uploadedBy uuid.Nil berarti berkasnya bukan hasil tindakan
// seorang pengguna. filename hanya menjadi nama unduhan: path-nya dibuang, dan
// jenis berkas tidak dibaca darinya.
//
// Bila kuota org tidak cukup, galatnya berjenis appkit.KindQuotaExceeded.
func (s *Service) Save(ctx context.Context, org uuid.UUID, owner Owner, filename string, uploadedBy uuid.UUID, r io.Reader) (File, error) {
	if org == uuid.Nil {
		return File{}, errors.New("attachments: organization kosong")
	}
	if err := owner.validate(); err != nil {
		return File{}, err
	}
	// Dibaca satu byte lebih dari batas: begitu terlampaui, berhenti membaca.
	data, err := io.ReadAll(io.LimitReader(r, s.maxBytes+1))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return File{}, s.tooLarge()
		}
		return File{}, fmt.Errorf("attachments: membaca berkas: %w", err)
	}
	switch {
	case len(data) == 0:
		return File{}, appkit.Validation("Berkas kosong.")
	case int64(len(data)) > s.maxBytes:
		return File{}, s.tooLarge()
	}

	f := File{ID: uuid.New(), Owner: owner, Filename: cleanFilename(filename), Size: int64(len(data))}
	// Jenis dikenali dari isinya; nama berkas dan header Content-Type milik
	// klien tidak dipakai. Parameternya ("; charset=utf-8") tidak disimpan.
	f.ContentType, _, _ = strings.Cut(http.DetectContentType(data), ";")
	if !slices.Contains(s.types, f.ContentType) {
		return File{}, appkit.Validation(s.wrongType)
	}
	if uploadedBy != uuid.Nil {
		f.UploadedBy = &uploadedBy
	}
	if err := s.checkQuota(ctx, org, f.Size); err != nil {
		return File{}, err
	}
	sum := sha256.Sum256(data)

	// Isi dulu, baru barisnya: baris tanpa isi adalah lampiran yang gagal
	// dibuka, sedangkan isi tanpa baris hanya sampah yang tidak terlihat.
	k := key(org, f.ID)
	if err := s.store.Put(ctx, k, bytes.NewReader(data), f.Size, f.ContentType); err != nil {
		return File{}, fmt.Errorf("attachments: menyimpan isi berkas: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO appkit_attachments
			(id, organization_id, owner_type, owner_id, filename, content_type, size_bytes, sha256, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at`,
		f.ID, org, owner.Type, owner.ID, f.Filename, f.ContentType, f.Size, hex.EncodeToString(sum[:]), f.UploadedBy).
		Scan(&f.CreatedAt)
	if err != nil {
		_ = s.store.Delete(context.WithoutCancel(ctx), k)
		return File{}, fmt.Errorf("attachments: mencatat berkas: %w", err)
	}
	return f, nil
}

// cleanFilename merapikan nama berkas kiriman klien menjadi nama unduhan:
// path-nya dibuang, karakter kendali dihapus, dan panjangnya dibatasi. Nama
// yang kosong sesudahnya menjadi "berkas".
func cleanFilename(name string) string {
	name = strings.ToValidUTF8(name, "")
	// Sebagian klien mengirim path lengkap, dan pemisahnya dapat "\".
	name = strings.ReplaceAll(name, `\`, "/")
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > maxFilename {
		name = strings.TrimSpace(string(runes[:maxFilename]))
	}
	if name == "" || name == "." || name == ".." {
		return fallbackFilename
	}
	return name
}

// Usage menjumlahkan pemakaian org oleh berkas package ini saja; berkas media
// tidak ikut. Jumlahnya sama apa pun penyimpanannya, karena data tentang
// berkas selalu di tabel.
func (s *Service) Usage(ctx context.Context, org uuid.UUID) (media.Usage, error) {
	var u media.Usage
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(size_bytes), 0)::bigint, COUNT(*)::int
		FROM appkit_attachments WHERE organization_id = $1`, org).Scan(&u.Bytes, &u.Files)
	if err != nil {
		return media.Usage{}, fmt.Errorf("attachments: menghitung pemakaian: %w", err)
	}
	return u, nil
}

// checkQuota menolak berkas berukuran size bila kuota org tidak cukup.
func (s *Service) checkQuota(ctx context.Context, org uuid.UUID, size int64) error {
	if s.quota == nil {
		return nil
	}
	limit, err := s.quota(ctx, org)
	if err != nil {
		return fmt.Errorf("attachments: membaca kuota: %w", err)
	}
	switch {
	case limit < 0:
		return nil
	case limit == 0:
		return appkit.QuotaExceeded(appkit.LimitStorage, "Paket Anda tidak menyertakan penyimpanan berkas.")
	}
	usage, err := s.Usage(ctx, org)
	if err != nil {
		return err
	}
	used := usage.Bytes
	if s.otherUsage != nil {
		// Batasnya satu untuk seluruh berkas organization itu, bukan satu per
		// package.
		other, err := s.otherUsage(ctx, org)
		if err != nil {
			return fmt.Errorf("attachments: membaca pemakaian lain: %w", err)
		}
		used += other
	}
	if used+size > limit {
		return appkit.QuotaExceeded(appkit.LimitStorage, fmt.Sprintf(
			"Penyimpanan penuh: %s dari %s sudah terpakai. Hapus berkas yang tidak dipakai, atau naikkan paket.",
			humanSize(used), humanSize(limit)))
	}
	return nil
}

func (s *Service) tooLarge() error {
	return appkit.Validation(fmt.Sprintf("Ukuran berkas maksimal %s.", humanSize(s.maxBytes)))
}

// humanSize menulis ukuran dengan satuan yang wajar dibaca: bilangan bulat
// bila pas, satu angka desimal bila tidak. Sama dengan yang dipakai media,
// supaya pesan kuotanya seragam.
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
	SELECT id, owner_type, owner_id, filename, content_type, size_bytes, uploaded_by, created_at
	FROM appkit_attachments`

func scanFile(row pgx.Row) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.Owner.Type, &f.Owner.ID, &f.Filename, &f.ContentType, &f.Size, &f.UploadedBy, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, errNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("attachments: membaca berkas: %w", err)
	}
	return f, nil
}

// List mengembalikan seluruh lampiran owner milik org, TERLAMA dulu. Owner
// tanpa lampiran — termasuk owner milik organization lain — dijawab daftar
// kosong.
func (s *Service) List(ctx context.Context, org uuid.UUID, owner Owner) ([]File, error) {
	if err := owner.validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, selectFile+`
		WHERE organization_id = $1 AND owner_type = $2 AND owner_id = $3
		ORDER BY created_at, id`, org, owner.Type, owner.ID)
	if err != nil {
		return nil, fmt.Errorf("attachments: membaca daftar berkas: %w", err)
	}
	defer rows.Close()
	files := []File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attachments: membaca daftar berkas: %w", err)
	}
	return files, nil
}

// Get membaca data berkas id, lampiran owner milik org.
//
// owner adalah dokumen yang izinnya SUDAH diperiksa pemanggil; id boleh
// datang langsung dari URL. Berkas milik organization lain, dan berkas
// dokumen lain di organization yang sama, dijawab "tidak ditemukan" — sama
// persis dengan berkas yang tidak ada.
func (s *Service) Get(ctx context.Context, org uuid.UUID, owner Owner, id uuid.UUID) (File, error) {
	if err := owner.validate(); err != nil {
		return File{}, err
	}
	return scanFile(s.pool.QueryRow(ctx, selectFile+`
		WHERE organization_id = $1 AND owner_type = $2 AND owner_id = $3 AND id = $4`,
		org, owner.Type, owner.ID, id))
}

// Open membuka berkas id, lampiran owner milik org; pemanggil wajib menutup
// isinya. Open SELALU menyaring organization dan owner — tidak ada jalur baca
// tanpa keduanya di package ini — dan yang tidak cocok dijawab "tidak
// ditemukan", seperti Get.
func (s *Service) Open(ctx context.Context, org uuid.UUID, owner Owner, id uuid.UUID) (File, io.ReadCloser, error) {
	f, err := s.Get(ctx, org, owner, id)
	if err != nil {
		return File{}, nil, err
	}
	rc, err := s.store.Open(ctx, key(org, f.ID))
	if err != nil {
		return File{}, nil, fmt.Errorf("attachments: membuka isi berkas %s: %w", f.ID, err)
	}
	return f, rc, nil
}

// Delete menghapus berkas id, lampiran owner milik org. Berkas yang sudah
// tidak ada — atau milik organization lain, atau lampiran dokumen lain —
// bukan galat, dan tidak ada yang terhapus. Berkas yang masih dirujuk tabel
// lain ditolak database; lepaskan rujukannya lebih dulu.
func (s *Service) Delete(ctx context.Context, org uuid.UUID, owner Owner, id uuid.UUID) error {
	if err := owner.validate(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM appkit_attachments
		WHERE organization_id = $1 AND owner_type = $2 AND owner_id = $3 AND id = $4`,
		org, owner.Type, owner.ID, id)
	if err != nil {
		return fmt.Errorf("attachments: menghapus berkas: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := s.store.Delete(ctx, key(org, id)); err != nil {
		return fmt.Errorf("attachments: menghapus isi berkas: %w", err)
	}
	return nil
}

// DeleteOwner menghapus seluruh lampiran owner milik org dan mengembalikan
// jumlahnya, untuk dipanggil saat dokumennya dihapus. Owner tanpa lampiran
// bukan galat.
//
// Barisnya dihapus lebih dulu, dalam satu perintah. Isi yang sesudah itu gagal
// dihapus dilaporkan sebagai galat, tetapi hanya sampah yang tidak terlihat:
// berkasnya sudah tidak dapat dibuka.
func (s *Service) DeleteOwner(ctx context.Context, org uuid.UUID, owner Owner) (int, error) {
	if err := owner.validate(); err != nil {
		return 0, err
	}
	rows, err := s.pool.Query(ctx, `
		DELETE FROM appkit_attachments
		WHERE organization_id = $1 AND owner_type = $2 AND owner_id = $3
		RETURNING id`, org, owner.Type, owner.ID)
	if err != nil {
		return 0, fmt.Errorf("attachments: menghapus berkas: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("attachments: menghapus berkas: %w", err)
	}
	var failed []error
	for _, id := range ids {
		if err := s.store.Delete(ctx, key(org, id)); err != nil {
			failed = append(failed, err)
		}
	}
	if err := errors.Join(failed...); err != nil {
		return len(ids), fmt.Errorf("attachments: menghapus isi berkas: %w", err)
	}
	return len(ids), nil
}

// Serve menulis berkas f sebagai jawaban UNDUHAN. Ini pembantu untuk handler
// milik produk yang SUDAH memeriksa izin; Serve tidak memeriksa apa pun.
//
// content biasanya hasil Open; pemanggil yang menutupnya. Berkas tidak pernah
// ditampilkan di dalam halaman dan tidak disimpan peramban maupun proxy:
// isinya kiriman pengguna, dan hak melihatnya dapat dicabut kapan saja.
func Serve(w http.ResponseWriter, r *http.Request, f File, content io.Reader) {
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	h.Set("Content-Disposition", disposition(f.Filename))
	h.Set("Cache-Control", "private, no-store")
	// Jenisnya sudah diperiksa saat disimpan; dua header ini memastikan
	// peramban tidak menafsirkannya sebagai hal lain.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, content)
}

// disposition menulis header Content-Disposition untuk unduhan bernama
// filename. Nama berkas berasal dari pengguna: tanda kutip di-escape dan huruf
// di luar ASCII dikodekan (RFC 2231), sehingga nama apa pun tidak dapat
// menyisipkan parameter atau header lain.
func disposition(filename string) string {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); v != "" {
		return v
	}
	return "attachment"
}
