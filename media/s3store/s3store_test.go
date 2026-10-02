package s3store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media/s3store"
)

func TestNewRequires(t *testing.T) {
	full := s3store.Options{Region: "auto", Bucket: "b", AccessKeyID: "id", SecretAccessKey: "secret"}
	if _, err := s3store.New(full); err != nil {
		t.Fatalf("New dengan isian lengkap: %v", err)
	}
	for name, strip := range map[string]func(*s3store.Options){
		"bucket":     func(o *s3store.Options) { o.Bucket = "" },
		"access key": func(o *s3store.Options) { o.AccessKeyID = "" },
		"secret":     func(o *s3store.Options) { o.SecretAccessKey = "" },
		"region":     func(o *s3store.Options) { o.Region = "" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := full
			strip(&opts)
			if _, err := s3store.New(opts); err == nil {
				t.Error("New lolos tanpa isian wajib")
			}
		})
	}
}

// Kredensial tidak boleh ikut ke log saat Store dicetak.
func TestStringHidesCredentials(t *testing.T) {
	store, err := s3store.New(s3store.Options{
		Region: "auto", Bucket: "berkas", Prefix: "media/", AccessKeyID: "kunci-akses", SecretAccessKey: "sangat-rahasia",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.String(); got != "s3://berkas/media" {
		t.Errorf("String = %q", got)
	}
}

// fakeS3 adalah layanan S3 seperlunya yang mencatat setiap permintaan, untuk
// memeriksa BENTUK permintaan yang dikirim: alamat, header, dan isinya.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests []*http.Request
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Clone(context.Background()))

	switch {
	case strings.HasPrefix(r.URL.Path, "/terlarang/"):
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>ditolak</Message></Error>`)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Path] = body
	case r.Method == http.MethodGet:
		body, ok := f.objects[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>tidak ada</Message></Error>`)
			return
		}
		_, _ = w.Write(body)
	case r.Method == http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeS3) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("tidak ada permintaan yang tercatat")
	}
	return f.requests[len(f.requests)-1]
}

// onlyReader menyembunyikan Seek, seperti body sebuah request.
type onlyReader struct{ io.Reader }

func TestRequestShape(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	ctx := context.Background()

	newStore := func(t *testing.T, bucket string) *s3store.Store {
		t.Helper()
		store, err := s3store.New(s3store.Options{
			Endpoint: srv.URL, Region: "auto", Bucket: bucket, Prefix: "media/",
			AccessKeyID: "id", SecretAccessKey: "secret", PathStyle: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	store := newStore(t, "berkas")

	t.Run("put", func(t *testing.T) {
		if err := store.Put(ctx, "org/satu", strings.NewReader("isi"), 3, "image/png"); err != nil {
			t.Fatalf("Put: %v", err)
		}
		req := fake.last(t)
		// Prefix ikut di depan key, dan bucket di path karena PathStyle.
		if req.Method != http.MethodPut || req.URL.Path != "/berkas/media/org/satu" {
			t.Errorf("permintaan = %s %s", req.Method, req.URL.Path)
		}
		if got := req.Header.Get("Content-Type"); got != "image/png" {
			t.Errorf("Content-Type = %q", got)
		}
		if req.ContentLength != 3 {
			t.Errorf("Content-Length = %d, ingin 3", req.ContentLength)
		}
		// R2 dan layanan lain yang bukan AWS menolak header checksum tambahan
		// yang dikirim bawaan SDK. Permintaan ini tidak boleh membawanya.
		for name := range req.Header {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "x-amz-checksum-") || lower == "x-amz-sdk-checksum-algorithm" || lower == "x-amz-trailer" {
				t.Errorf("permintaan membawa header checksum %s", name)
			}
		}
		if got := req.Header.Get("Content-Encoding"); strings.Contains(got, "aws-chunked") {
			t.Errorf("Content-Encoding = %q; isi tidak boleh dikirim berpotongan", got)
		}
		if string(fake.objects["/berkas/media/org/satu"]) != "isi" {
			t.Errorf("isi tersimpan = %q", fake.objects["/berkas/media/org/satu"])
		}
	})

	t.Run("put dari pembaca yang tidak dapat diputar ulang", func(t *testing.T) {
		if err := store.Put(ctx, "org/dua", onlyReader{strings.NewReader("mengalir")}, 8, "image/webp"); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if string(fake.objects["/berkas/media/org/dua"]) != "mengalir" {
			t.Errorf("isi tersimpan = %q", fake.objects["/berkas/media/org/dua"])
		}
	})

	t.Run("open", func(t *testing.T) {
		rc, err := store.Open(ctx, "org/satu")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "isi" {
			t.Errorf("isi = %q", got)
		}
	})

	t.Run("open key yang tidak ada", func(t *testing.T) {
		if _, err := store.Open(ctx, "org/tidak-ada"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("galat = %v, ingin fs.ErrNotExist", err)
		}
	})

	// Hanya "key tidak ada" yang boleh dibaca sebagai tidak ada. Galat lain —
	// kredensial ditolak, bucket salah — harus tampil sebagai galat, bukan
	// membuat berkas tampak hilang.
	t.Run("open yang ditolak bukan tidak ada", func(t *testing.T) {
		_, err := newStore(t, "terlarang").Open(ctx, "org/satu")
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("galat = %v, ingin galat selain fs.ErrNotExist", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := store.Delete(ctx, "org/satu"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		req := fake.last(t)
		if req.Method != http.MethodDelete || req.URL.Path != "/berkas/media/org/satu" {
			t.Errorf("permintaan = %s %s", req.Method, req.URL.Path)
		}
		if _, ok := fake.objects["/berkas/media/org/satu"]; ok {
			t.Error("isi masih ada setelah Delete")
		}
	})
}

// Identitas layanan S3 di compose.dev.yaml dan CI. Hanya berlaku di sana.
const (
	testAccessKey = "appkit"
	testSecretKey = "appkit-secret"
)

// serverOptions mengembalikan Options untuk layanan S3 test dengan bucket
// baru yang dihapus saat test selesai. Test di-skip bila TEST_S3_ENDPOINT
// kosong; `make test` dan CI mengisinya.
func serverOptions(t *testing.T) s3store.Options {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT kosong; jalankan lewat `make test`")
	}
	opts := s3store.Options{
		Endpoint: endpoint, Region: "us-east-1", Bucket: "appkit-test-" + strings.ToLower(rand.Text()),
		AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey, PathStyle: true,
	}
	client := s3.New(s3.Options{
		Region:       opts.Region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(testAccessKey, testSecretKey, ""),
	})
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(opts.Bucket)}); err != nil {
		t.Fatalf("membuat bucket: %v", err)
	}
	t.Cleanup(func() {
		list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(opts.Bucket)})
		if err != nil {
			t.Errorf("membaca isi bucket %s: %v", opts.Bucket, err)
			return
		}
		for _, object := range list.Contents {
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(opts.Bucket), Key: object.Key}); err != nil {
				t.Errorf("menghapus %s: %v", aws.ToString(object.Key), err)
			}
		}
		if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(opts.Bucket)}); err != nil {
			t.Errorf("menghapus bucket %s: %v", opts.Bucket, err)
		}
	})
	return opts
}

func mustNew(t *testing.T, opts s3store.Options) *s3store.Store {
	t.Helper()
	store, err := s3store.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Terhadap layanan S3 sungguhan: tanda tangan permintaan diperiksa, dan
// header yang tidak dikenal ditolak.
func TestAgainstServer(t *testing.T) {
	opts := serverOptions(t)
	ctx := context.Background()
	opts.Prefix = "media/"
	store := mustNew(t, opts)

	if err := store.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := store.Open(ctx, "org/tidak-ada"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open key yang tidak ada = %v, ingin fs.ErrNotExist", err)
	}

	// Menulis ulang key yang sama menggantikan isinya.
	for _, content := range []string{"satu", "dua-dua"} {
		if err := store.Put(ctx, "org/k", strings.NewReader(content), int64(len(content)), "image/png"); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	rc, err := store.Open(ctx, "org/k")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "dua-dua" {
		t.Errorf("isi = %q, ingin tulisan terakhir", got)
	}

	// Prefix memisahkan isi: Store tanpa prefix tidak melihat key yang sama.
	bare := opts
	bare.Prefix = ""
	if _, err := mustNew(t, bare).Open(ctx, "org/k"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open tanpa prefix = %v, ingin fs.ErrNotExist", err)
	}

	// Menghapus yang sudah tidak ada bukan galat.
	for range 2 {
		if err := store.Delete(ctx, "org/k"); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}
	if _, err := store.Open(ctx, "org/k"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open setelah Delete = %v, ingin fs.ErrNotExist", err)
	}
}

// Salah konfigurasi harus ketahuan saat start, lewat Check.
func TestCheckCatchesMisconfiguration(t *testing.T) {
	opts := serverOptions(t)
	ctx := context.Background()

	for name, change := range map[string]func(*s3store.Options){
		"bucket tidak ada": func(o *s3store.Options) { o.Bucket = "appkit-test-tidak-ada" },
		"secret salah":     func(o *s3store.Options) { o.SecretAccessKey = "salah" },
	} {
		t.Run(name, func(t *testing.T) {
			wrong := opts
			change(&wrong)
			if err := mustNew(t, wrong).Check(ctx); err == nil {
				t.Error("Check lolos")
			}
		})
	}

	wrong := opts
	wrong.SecretAccessKey = "salah"
	err := mustNew(t, wrong).Put(ctx, "org/k", strings.NewReader("isi"), 3, "image/png")
	if err == nil {
		t.Fatal("Put dengan secret yang salah lolos")
	}
	if strings.Contains(err.Error(), "salah") {
		t.Errorf("galat memuat secret: %v", err)
	}
}

// media.Service di atas S3: isi berkas baru masuk ke bucket, bukan ke
// database, dan berkas yang isinya sudah di database sebelum berpindah tetap
// terbaca serta tetap dapat dihapus.
func TestMediaServiceOnS3(t *testing.T) {
	opts := serverOptions(t)
	pool := testdb.New(t)
	ctx := context.Background()
	org := uuid.New()

	image := append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)
	blobs := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM appkit_media_blobs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	read := func(s *media.Service, id uuid.UUID) []byte {
		t.Helper()
		_, rc, err := s.Open(ctx, id)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer rc.Close()
		content, _ := io.ReadAll(rc)
		return content
	}

	// Sebelum berpindah: isi di database.
	before, err := media.New(pool, testdb.Hooks(), media.Options{})
	if err != nil {
		t.Fatal(err)
	}
	old, err := before.Save(ctx, org, bytes.NewReader(image))
	if err != nil {
		t.Fatalf("Save ke database: %v", err)
	}
	if blobs() != 1 {
		t.Fatalf("isi di database = %d, ingin 1", blobs())
	}

	store := mustNew(t, opts)
	after, err := media.New(pool, testdb.Hooks(), media.Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := after.Save(ctx, org, bytes.NewReader(image))
	if err != nil {
		t.Fatalf("Save ke S3: %v", err)
	}
	if blobs() != 1 {
		t.Errorf("isi di database = %d setelah Save ke S3, ingin tetap 1", blobs())
	}
	rc, err := store.Open(ctx, org.String()+"/"+fresh.ID.String())
	if err != nil {
		t.Fatalf("isi berkas baru tidak ada di bucket: %v", err)
	}
	rc.Close()

	for name, id := range map[string]uuid.UUID{"lama": old.ID, "baru": fresh.ID} {
		if !bytes.Equal(read(after, id), image) {
			t.Errorf("isi berkas %s berbeda dari yang disimpan", name)
		}
	}
	usage, err := after.Usage(ctx, org)
	if err != nil || usage.Files != 2 || usage.Bytes != 2*int64(len(image)) {
		t.Errorf("Usage = %+v, %v", usage, err)
	}

	// Jalur publik menyajikan keduanya dari URL yang sama seperti sebelumnya.
	mux := http.NewServeMux()
	appkit.Register(mux, "", after.PublicRoutes()...)
	for name, f := range map[string]media.File{"lama": old, "baru": fresh} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, f.URL, nil))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), image) {
			t.Errorf("GET berkas %s = %d", name, rec.Code)
		}
	}

	for _, id := range []uuid.UUID{old.ID, fresh.ID} {
		if err := after.Delete(ctx, org, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if blobs() != 0 {
		t.Errorf("isi di database = %d setelah dihapus, ingin 0", blobs())
	}
	if _, err := store.Open(ctx, org.String()+"/"+fresh.ID.String()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("isi di bucket setelah dihapus = %v, ingin fs.ErrNotExist", err)
	}
}
