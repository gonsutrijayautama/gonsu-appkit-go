// Package s3store menyimpan isi berkas media di object storage yang
// berbicara API S3: Cloudflare R2, MinIO, AWS S3, dan sejenisnya.
//
// Ini pelaksana kedua media.Store, di samping media.DBStore. Data tentang
// berkasnya tetap di tabel appkit_media, jadi berpindah ke sini tidak
// mengubah skema, API, maupun URL berkas. Berkas lama yang isinya masih di
// database tetap terbaca: media.Service mencarinya di sana bila tidak ada di
// sini.
//
// Tidak ada kode khusus satu penyedia. Yang membedakan penyedia hanya Options:
//
//	R2:    Endpoint https://<akun>.r2.cloudflarestorage.com, Region "auto"
//	MinIO: Endpoint http://host:9000, PathStyle true
//	AWS:   Endpoint kosong, Region wilayah bucket-nya
//
// Package ini terpisah supaya produk yang tidak mengimpornya tidak ikut
// mengompilasi klien S3.
package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Options adalah alamat dan kredensial bucket. Nilainya diisi produk dari
// environment-nya sendiri; package ini tidak membaca environment.
type Options struct {
	// Endpoint adalah alamat layanan S3. Kosong: AWS S3 di Region.
	Endpoint string
	// Region wajib bagi klien S3. R2 memakai "auto".
	Region string
	// Bucket tempat seluruh berkas. Wajib, dan harus sudah ada.
	Bucket string
	// AccessKeyID dan SecretAccessKey adalah kredensial bucket. Wajib.
	AccessKeyID     string
	SecretAccessKey string
	// Prefix adalah awalan key di dalam bucket, mis. "media/". Boleh kosong.
	Prefix string
	// PathStyle memakai alamat https://host/bucket/key alih-alih
	// https://bucket.host/key. MinIO membutuhkannya.
	PathStyle bool
}

// Store adalah media.Store di atas API S3.
type Store struct {
	client *s3.Client
	bucket string
	prefix string
}

// New mengembalikan penyimpanan S3. Ia tidak menghubungi layanannya; panggil
// Check saat start supaya salah konfigurasi ketahuan sebelum unggahan pertama.
func New(opts Options) (*Store, error) {
	switch {
	case opts.Bucket == "":
		return nil, errors.New("s3store: Bucket wajib diisi")
	case opts.AccessKeyID == "" || opts.SecretAccessKey == "":
		return nil, errors.New("s3store: AccessKeyID dan SecretAccessKey wajib diisi")
	case opts.Region == "":
		return nil, errors.New("s3store: Region wajib diisi (R2: \"auto\")")
	}
	client := s3.New(s3.Options{
		Region:       opts.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretAccessKey, ""),
		UsePathStyle: opts.PathStyle,
		// Checksum tambahan hanya bila operasinya mewajibkan, dan itu ditulis
		// terang di sini: bawaan SDK saat konfigurasinya dimuat dari
		// environment adalah menyertakannya di setiap unggahan, sedangkan
		// sebagian layanan yang berbicara S3 — termasuk R2 — menolak header itu.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
	})
	return &Store{client: client, bucket: opts.Bucket, prefix: opts.Prefix}, nil
}

// Check memastikan bucket-nya terjangkau dengan kredensial ini.
func (s *Store) Check(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("s3store: bucket %s tidak terjangkau: %w", s.bucket, err)
	}
	return nil
}

func (s *Store) key(key string) *string { return aws.String(s.prefix + key) }

// Put menyimpan size byte dari r di bawah key.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	// Klien S3 harus dapat memutar ulang isinya: untuk tanda tangan permintaan
	// dan untuk mengulang kiriman yang gagal. Yang tidak dapat diputar dibaca
	// ke memori — ukurannya sudah dibatasi media.Options.MaxBytes.
	body, ok := r.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("s3store: membaca %s: %w", key, err)
		}
		body = bytes.NewReader(data)
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           s.key(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("s3store: menyimpan %s: %w", key, err)
	}
	return nil
}

// Open membuka isi key. Key yang tidak ada mengembalikan galat yang memenuhi
// errors.Is(err, fs.ErrNotExist).
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: s.key(key)})
	if err != nil {
		if _, missing := errors.AsType[*types.NoSuchKey](err); missing {
			return nil, fmt.Errorf("s3store: isi %s: %w", key, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("s3store: membuka %s: %w", key, err)
	}
	return out.Body, nil
}

// Delete menghapus key. Key yang sudah tidak ada bukan galat: S3 menjawab
// berhasil untuk keduanya.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: s.key(key)})
	if err != nil {
		return fmt.Errorf("s3store: menghapus %s: %w", key, err)
	}
	return nil
}

// String menyebut bucket-nya, untuk log saat start. Kredensial tidak ikut.
func (s *Store) String() string {
	return "s3://" + s.bucket + "/" + strings.TrimSuffix(s.prefix, "/")
}
