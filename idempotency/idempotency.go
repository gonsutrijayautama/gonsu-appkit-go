// Package idempotency membuat mutasi yang diulang client — karena timeout,
// koneksi putus, atau tombol ditekan dua kali — tidak menghasilkan dokumen
// ganda.
//
// Alurnya bersama transaksi dokumen:
//
//	Replay   (di luar transaksi)   → sudah tersimpan? putar ulang response-nya byte per byte
//	Claim    (di dalam transaksi)  → key dikunci bersama dokumen yang dibuat
//	Complete (transaksi yang sama) → response disimpan, lalu commit
//
// Karena klaim, dokumen, dan response-nya di-commit bersama, tidak ada
// keadaan "dokumen sudah ada tetapi key belum tercatat" yang membuat retry
// menggandakannya. Transaksi yang dibatalkan melepas klaimnya: key itu dapat
// dipakai lagi.
//
// Di modul pemanggil bentuknya seperti ini; org dibaca dari
// Hooks.Organization, key dari Key, dan fingerprint dari Fingerprint:
//
//	scope := idempotency.Scope{OrganizationID: org, Endpoint: "POST /v1/invoices", Key: key, Fingerprint: fingerprint}
//	if resp, err := idem.Replay(ctx, scope); err != nil || resp != nil {
//		return resp, err
//	}
//	tx, err := pool.Begin(ctx)
//	…
//	defer func() { _ = tx.Rollback(ctx) }()
//	claimed, err := idem.Claim(ctx, tx, scope)
//	…
//	if !claimed {
//		// Request lain meng-commit key yang sama lebih dulu. Transaksi ini
//		// dilepas sebelum Replay mengambil koneksi lain dari pool.
//		_ = tx.Rollback(ctx)
//		if resp, err := idem.Replay(ctx, scope); err != nil || resp != nil {
//			return resp, err
//		}
//		return nil, appkit.IdempotencyConflict("Permintaan dengan kunci yang sama sedang diproses.")
//	}
//	… membuat dokumen di tx, menyusun body response …
//	resp := idempotency.Response{Status: http.StatusCreated, Body: body}
//	if err := idem.Complete(ctx, tx, scope, resp); err != nil {
//		return nil, err
//	}
//	return &resp, tx.Commit(ctx)
//
// Yang perlu diketahui sebelum memakainya:
//
//   - response yang membawa RAHASIA — misalnya sandi sementara atau token
//     yang hanya ditampilkan sekali — tidak boleh lewat modul ini. Response
//     yang tersimpan diputar ulang, jadi isinya ikut tersimpan di database
//     selama key-nya hidup;
//   - yang disimpan hanya response mutasi yang BERHASIL di-commit. Galat
//     isian dan galat lain membatalkan transaksinya, dan key-nya bebas
//     dipakai untuk mencoba lagi;
//   - key berlaku per organization dan per endpoint, selama Options.TTL.
//     Sesudahnya key yang sama diperlakukan sebagai permintaan baru;
//   - library ini tidak menjalankan pekerjaan latar. Baris kedaluwarsa
//     dihapus produk dengan memanggil DeleteExpired secara berkala.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// Header adalah nama header idempotency.
const Header = "Idempotency-Key"

// DefaultTTL adalah lama bawaan sebuah key hidup: selama itu retry dengan key
// yang sama diputar ulang.
const DefaultTTL = 24 * time.Hour

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,200}$`)

// Key membaca Idempotency-Key. Wajib pada endpoint yang memakainya.
func Key(r *http.Request) (string, error) {
	k := r.Header.Get(Header)
	if k == "" {
		return "", appkit.Validation("Header Idempotency-Key wajib diisi.")
	}
	if !keyPattern.MatchString(k) {
		return "", appkit.Validation("Header Idempotency-Key tidak valid: 8–200 karakter huruf, angka, atau . _ : -.")
	}
	return k, nil
}

// Fingerprint adalah sidik jari body request: SHA-256 dalam heksadesimal. Key
// sama dengan body berbeda adalah kesalahan client, bukan pengulangan.
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Scope mengidentifikasi satu key: per organization dan endpoint.
type Scope struct {
	// OrganizationID diisi modul pemanggil dengan yang dibacanya dari
	// Hooks.Organization, tidak pernah dari body, query, atau header. Key milik
	// organization lain tidak terlihat: ia dijawab seperti belum pernah
	// dipakai.
	OrganizationID uuid.UUID
	// Endpoint adalah nama tetap operasinya, mis. "POST /v1/invoices". Key yang
	// sama di dua endpoint adalah dua key.
	Endpoint string
	// Key adalah hasil Key.
	Key string
	// Fingerprint adalah hasil Fingerprint atas body request.
	Fingerprint string
}

// Batas isi, sama dengan constraint kolomnya.
const (
	maxEndpoint    = 200
	maxKey         = 200
	maxFingerprint = 200
	minStatus      = 100
	maxStatus      = 599
)

// validate memeriksa sc. Galatnya galat pemrogram, bukan galat isian pengguna.
func (sc Scope) validate() error {
	filled := func(v string, limit int) bool { return v != "" && utf8.RuneCountInString(v) <= limit }
	switch {
	case sc.OrganizationID == uuid.Nil:
		return errors.New("idempotency: organization kosong")
	case !filled(sc.Endpoint, maxEndpoint):
		return fmt.Errorf("idempotency: Scope.Endpoint wajib diisi, maksimal %d karakter", maxEndpoint)
	case !filled(sc.Key, maxKey):
		return fmt.Errorf("idempotency: Scope.Key wajib diisi, maksimal %d karakter", maxKey)
	case !filled(sc.Fingerprint, maxFingerprint):
		return fmt.Errorf("idempotency: Scope.Fingerprint wajib diisi, maksimal %d karakter", maxFingerprint)
	}
	return nil
}

// Response adalah response yang disimpan dan diputar ulang byte per byte.
type Response struct {
	Status int
	Body   []byte
	// Replayed true bila response ini dibaca dari simpanan, bukan hasil
	// permintaan ini. Diisi Replay; diabaikan Complete.
	Replayed bool
}

// Options mengatur Service. Nilai kosong memakai bawaan.
type Options struct {
	// TTL adalah lama sebuah key hidup sejak diklaim. Kosong: DefaultTTL.
	TTL time.Duration
}

// Service menyimpan dan memutar ulang response mutasi.
type Service struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// New mengembalikan service idempotency. Service ini tidak memakai pengait:
// organization diserahkan modul pemanggil lewat Scope, dan izin sudah
// diperiksa pemanggil sebelum sampai ke sini.
func New(pool *pgxpool.Pool, opts Options) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("idempotency: pool wajib diisi")
	case opts.TTL < 0:
		return nil, errors.New("idempotency: Options.TTL tidak boleh negatif")
	case opts.TTL == 0:
		opts.TTL = DefaultTTL
	}
	return &Service{pool: pool, ttl: opts.TTL}, nil
}

var (
	errMismatch   = appkit.IdempotencyConflict("Idempotency-Key ini sudah dipakai untuk permintaan dengan isi berbeda.")
	errInProgress = appkit.IdempotencyConflict("Permintaan dengan Idempotency-Key ini masih diproses.")
)

// Replay mengembalikan response tersimpan untuk key ini, atau nil bila key
// belum pernah dipakai atau sudah kedaluwarsa. Dipanggil di luar transaksi,
// sebelum Claim.
//
// Galatnya berjenis appkit.KindIdempotencyConflict bila key yang sama
// tersimpan dengan sidik jari berbeda, dan bila key sudah diklaim tetapi
// response-nya belum tersimpan.
func (s *Service) Replay(ctx context.Context, sc Scope) (*Response, error) {
	if err := sc.validate(); err != nil {
		return nil, err
	}
	var (
		fingerprint string
		status      *int
		body        []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT request_fingerprint, response_status, response_body
		FROM appkit_idempotency_keys
		WHERE organization_id = $1 AND endpoint = $2 AND idempotency_key = $3
		  AND expires_at > now()`,
		sc.OrganizationID, sc.Endpoint, sc.Key).Scan(&fingerprint, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("idempotency: membaca key: %w", err)
	}
	if fingerprint != sc.Fingerprint {
		return nil, errMismatch
	}
	if status == nil {
		return nil, errInProgress
	}
	return &Response{Status: *status, Body: body, Replayed: true}, nil
}

// Claim mengunci key di dalam transaksi tx milik pemanggil, yaitu transaksi
// dokumen yang dibuat mutasinya. false berarti request lain sudah meng-commit
// key yang sama lebih dulu dan key itu masih hidup: pemanggil membatalkan
// transaksinya, lalu memanggil Replay. Urutannya penting: Replay mengambil
// koneksi lain dari pool, dan transaksi yang masih terbuka menahan satu.
//
// Dua klaim bersamaan atas key yang sama bergiliran: yang kedua menunggu
// transaksi yang pertama selesai, lalu mendapat false bila yang pertama
// commit, atau true bila yang pertama dibatalkan. Baris yang sudah
// kedaluwarsa diklaim ulang, dan response lamanya dibuang.
//
// Claim tidak membandingkan sidik jari; yang menjawab "isi berbeda" adalah
// Replay.
func (s *Service) Claim(ctx context.Context, tx pgx.Tx, sc Scope) (bool, error) {
	if err := sc.validate(); err != nil {
		return false, err
	}
	if tx == nil {
		return false, errors.New("idempotency: transaksi wajib diisi")
	}
	// Baris tersentuh hanya bila key baru diklaim — atau diklaim ulang karena
	// yang lama sudah kedaluwarsa. Tanpa baris berarti key yang sama masih
	// hidup. Kedaluwarsanya dihitung dengan jam database, jam yang sama dengan
	// yang membandingkannya.
	tag, err := tx.Exec(ctx, `
		INSERT INTO appkit_idempotency_keys (organization_id, endpoint, idempotency_key, request_fingerprint, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval)
		ON CONFLICT (organization_id, endpoint, idempotency_key) DO UPDATE
		    SET request_fingerprint = EXCLUDED.request_fingerprint,
		        response_status     = NULL,
		        response_body       = NULL,
		        created_at          = now(),
		        expires_at          = EXCLUDED.expires_at
		    WHERE appkit_idempotency_keys.expires_at <= now()`,
		sc.OrganizationID, sc.Endpoint, sc.Key, sc.Fingerprint, s.ttl)
	if err != nil {
		return false, fmt.Errorf("idempotency: mengklaim key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Complete menyimpan response di transaksi tx yang sama dengan Claim dan
// dokumennya. Pemanggil meng-commit sesudahnya.
//
// Complete hanya mengisi key yang response-nya belum tersimpan. Memanggilnya
// tanpa Claim yang berhasil — misalnya setelah Claim menjawab false — adalah
// galat, dan response yang sudah tersimpan tidak tertimpa.
func (s *Service) Complete(ctx context.Context, tx pgx.Tx, sc Scope, resp Response) error {
	if err := sc.validate(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("idempotency: transaksi wajib diisi")
	}
	if resp.Status < minStatus || resp.Status > maxStatus {
		return fmt.Errorf("idempotency: Response.Status %d tidak sah", resp.Status)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE appkit_idempotency_keys
		SET response_status = $4, response_body = $5
		WHERE organization_id = $1 AND endpoint = $2 AND idempotency_key = $3
		  AND response_status IS NULL`,
		sc.OrganizationID, sc.Endpoint, sc.Key, resp.Status, resp.Body)
	if err != nil {
		return fmt.Errorf("idempotency: menyimpan response: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("idempotency: Complete tanpa Claim yang berhasil di transaksi ini")
	}
	return nil
}

// DeleteExpired menghapus baris yang sudah kedaluwarsa dan mengembalikan
// jumlahnya. Library ini tidak menjalankan pekerjaan latar: produk
// memanggilnya secara berkala, misalnya sekali sehari. Tanpa itu tabelnya
// terus bertambah, walau baris kedaluwarsa tetap diperlakukan seperti tidak
// ada.
//
// PENGECUALIAN dari aturan "setiap query menyaring organization_id":
// penghapusan ini mencakup SEMUA organization. Ia pekerjaan perawatan yang
// dipanggil kode server di luar permintaan pengguna, tidak membaca atau
// mengembalikan isi baris mana pun selain jumlahnya, dan yang dihapusnya
// hanya baris yang sudah tidak berlaku bagi organization pemiliknya. Jangan
// memanggilnya dari handler permintaan.
func (s *Service) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM appkit_idempotency_keys WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("idempotency: menghapus key kedaluwarsa: %w", err)
	}
	return tag.RowsAffected(), nil
}
