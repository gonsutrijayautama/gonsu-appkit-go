package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store menyimpan ISI berkas. Data tentang berkasnya (jenis, ukuran, hash)
// selalu ada di tabel appkit_media, apa pun penyimpanannya — jadi mengganti
// Store tidak mengubah skema, API, maupun URL publik berkas.
//
// Implementasi wajib aman dipanggil bersamaan.
type Store interface {
	// Put menyimpan size byte dari r di bawah key. Menulis ulang key yang
	// sama menggantikan isinya.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Open membuka isi key. Key yang tidak ada mengembalikan galat yang
	// memenuhi errors.Is(err, fs.ErrNotExist).
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete menghapus key. Key yang sudah tidak ada bukan galat.
	Delete(ctx context.Context, key string) error
}

// DBStore menyimpan isi berkas di tabel appkit_media_blobs. Ini penyimpanan
// bawaan: tidak butuh layanan lain, jadi cocok untuk self-host dan pemasangan
// tanpa object storage.
//
// Akibatnya isi berkas ikut membesarkan backup database, sehingga batas
// ukuran (Options.MaxBytes) sengaja kecil. Untuk object storage, lihat
// package s3store.
type DBStore struct {
	pool *pgxpool.Pool
}

// NewDBStore mengembalikan penyimpanan di database.
func NewDBStore(pool *pgxpool.Pool) *DBStore { return &DBStore{pool: pool} }

func (s *DBStore) Put(ctx context.Context, key string, r io.Reader, _ int64, _ string) error {
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO appkit_media_blobs (key, content) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET content = EXCLUDED.content`, key, content)
	return err
}

func (s *DBStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	var content []byte
	err := s.pool.QueryRow(ctx, `SELECT content FROM appkit_media_blobs WHERE key = $1`, key).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("media: isi %s: %w", key, fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (s *DBStore) Delete(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM appkit_media_blobs WHERE key = $1`, key)
	return err
}

// withFallback menyimpan ke primary dan membaca dari primary, lalu dari
// fallback bila berkasnya tidak ada di primary. Dipakai Service saat
// penyimpanannya bukan database: berkas yang isinya tersimpan di database
// sebelum penyimpanan itu dipasang tetap terbaca.
type withFallback struct {
	primary, fallback Store
}

func (s withFallback) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	return s.primary.Put(ctx, key, r, size, contentType)
}

func (s withFallback) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.primary.Open(ctx, key)
	if errors.Is(err, fs.ErrNotExist) {
		return s.fallback.Open(ctx, key)
	}
	return rc, err
}

// Delete menghapus di keduanya: tidak ada yang mencatat di mana sebuah berkas
// lama berada, dan menghapus yang tidak ada bukan galat.
func (s withFallback) Delete(ctx context.Context, key string) error {
	return errors.Join(s.primary.Delete(ctx, key), s.fallback.Delete(ctx, key))
}
