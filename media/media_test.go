package media_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Kepala berkas WebP yang cukup untuk dikenali; isinya tidak didekode.
var webpBytes = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)

func newService(t *testing.T, opts media.Options) *media.Service {
	t.Helper()
	s, err := media.New(testdb.New(t), testdb.Hooks(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func wantValidation(t *testing.T, err error, contains string) {
	t.Helper()
	e, ok := errors.AsType[*appkit.Error](err)
	if !ok || e.Kind != appkit.KindValidation {
		t.Fatalf("galat = %v, ingin galat validasi", err)
	}
	if !strings.Contains(e.Message, contains) {
		t.Errorf("pesan = %q, ingin memuat %q", e.Message, contains)
	}
}

func TestSaveAndOpen(t *testing.T) {
	s := newService(t, media.Options{})
	ctx := context.Background()
	org := uuid.New()

	for name, tc := range map[string]struct {
		data          []byte
		contentType   string
		width, height int
	}{
		"png":  {pngBytes(t, 40, 20), "image/png", 40, 20},
		"jpeg": {jpegBytes(t, 16, 8), "image/jpeg", 16, 8},
		"webp": {webpBytes, "image/webp", 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := s.Save(ctx, org, bytes.NewReader(tc.data))
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if f.ContentType != tc.contentType || f.Width != tc.width || f.Height != tc.height || f.Size != int64(len(tc.data)) {
				t.Errorf("berkas = %+v", f)
			}
			if f.URL != "/media/"+f.ID.String() {
				t.Errorf("URL = %q", f.URL)
			}

			got, rc, err := s.Open(ctx, f.ID)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer rc.Close()
			content, _ := io.ReadAll(rc)
			if !bytes.Equal(content, tc.data) || got.ID != f.ID {
				t.Error("isi yang dibuka berbeda dari yang disimpan")
			}
		})
	}
}

func TestSaveRejects(t *testing.T) {
	s := newService(t, media.Options{MaxBytes: 4 << 10})
	ctx := context.Background()
	org := uuid.New()

	// Jenis dikenali dari isinya: SVG dan HTML ditolak apa pun namanya.
	for name, data := range map[string]string{
		"svg":  `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"html": `<!doctype html><html><body>halo</body></html>`,
		"teks": "bukan gambar",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Save(ctx, org, strings.NewReader(data))
			wantValidation(t, err, "PNG, JPEG, atau WebP")
		})
	}

	t.Run("kosong", func(t *testing.T) {
		_, err := s.Save(ctx, org, strings.NewReader(""))
		wantValidation(t, err, "kosong")
	})
	t.Run("terlalu besar", func(t *testing.T) {
		_, err := s.Save(ctx, org, bytes.NewReader(make([]byte, 4<<10+1)))
		wantValidation(t, err, "maksimal 4 KB")
	})
	t.Run("png rusak", func(t *testing.T) {
		_, err := s.Save(ctx, org, bytes.NewReader(pngBytes(t, 8, 8)[:20]))
		wantValidation(t, err, "rusak")
	})
	t.Run("tanpa organization", func(t *testing.T) {
		if _, err := s.Save(ctx, uuid.Nil, bytes.NewReader(pngBytes(t, 8, 8))); err == nil {
			t.Error("Save tanpa organization lolos")
		}
	})
}

// Get dan Delete menyaring organization: berkas milik organization lain
// dijawab "tidak ditemukan" dan tidak dapat dihapus.
func TestTenantIsolation(t *testing.T) {
	s := newService(t, media.Options{})
	ctx := context.Background()
	owner, other := uuid.New(), uuid.New()

	f, err := s.Save(ctx, owner, bytes.NewReader(pngBytes(t, 8, 8)))
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.Get(ctx, other, f.ID)
	if e, ok := errors.AsType[*appkit.Error](err); !ok || e.Kind != appkit.KindNotFound {
		t.Errorf("Get lintas organization = %v, ingin tidak ditemukan", err)
	}
	if err := s.Delete(ctx, other, f.ID); err != nil {
		t.Fatalf("Delete lintas organization: %v", err)
	}
	if _, err := s.Get(ctx, owner, f.ID); err != nil {
		t.Errorf("berkas terhapus oleh organization lain: %v", err)
	}

	if err := s.Delete(ctx, owner, f.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := s.Open(ctx, f.ID); err == nil {
		t.Error("berkas masih dapat dibuka setelah dihapus")
	}
	// Menghapus yang sudah tidak ada bukan galat.
	if err := s.Delete(ctx, owner, f.ID); err != nil {
		t.Errorf("Delete kedua: %v", err)
	}
}

func TestDBStore(t *testing.T) {
	store := media.NewDBStore(testdb.New(t))
	ctx := context.Background()

	if _, err := store.Open(ctx, "tidak-ada"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open key yang tidak ada = %v, ingin fs.ErrNotExist", err)
	}
	for _, content := range []string{"satu", "dua"} {
		if err := store.Put(ctx, "k", strings.NewReader(content), int64(len(content)), "image/png"); err != nil {
			t.Fatal(err)
		}
	}
	rc, err := store.Open(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "dua" {
		t.Errorf("isi = %q, ingin tulisan terakhir", got)
	}
	for range 2 {
		if err := store.Delete(ctx, "k"); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}
}

func TestPublicRoute(t *testing.T) {
	s := newService(t, media.Options{})
	data := pngBytes(t, 8, 8)
	f, err := s.Save(context.Background(), uuid.New(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	appkit.Register(mux, "", s.PublicRoutes()...)
	get := func(path string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header = header
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Tanpa sesi sama sekali: ini jalur publik.
	rec := get(f.URL, http.Header{})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("GET %s = %d", f.URL, rec.Code)
	}
	for header, want := range map[string]string{
		"Content-Type":            "image/png",
		"Cache-Control":           "public, max-age=31536000, immutable",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, ingin %q", header, got, want)
		}
	}

	etag := rec.Header().Get("ETag")
	if rec := get(f.URL, http.Header{"If-None-Match": {etag}}); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match = %d dengan %d byte, ingin 304 tanpa isi", rec.Code, rec.Body.Len())
	}

	for _, path := range []string{"/media/" + uuid.NewString(), "/media/bukan-uuid"} {
		if rec := get(path, http.Header{}); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, ingin 404", path, rec.Code)
		}
	}
}

func wantQuotaExceeded(t *testing.T, err error, contains string) {
	t.Helper()
	e, ok := errors.AsType[*appkit.Error](err)
	if !ok || e.Kind != appkit.KindQuotaExceeded {
		t.Fatalf("galat = %v, ingin galat kuota penuh", err)
	}
	if !strings.Contains(e.Message, contains) {
		t.Errorf("pesan = %q, ingin memuat %q", e.Message, contains)
	}
}

// sized mengembalikan "WebP" berukuran tepat n byte: kepala yang cukup untuk
// dikenali, sisanya nol.
func sized(n int) []byte {
	data := make([]byte, n)
	copy(data, "RIFF\x24\x00\x00\x00WEBPVP8 ")
	return data
}

func TestQuota(t *testing.T) {
	limited, unlimited, none := uuid.New(), uuid.New(), uuid.New()
	s := newService(t, media.Options{
		Quota: func(_ context.Context, org uuid.UUID) (int64, error) {
			switch org {
			case limited:
				return 3 << 10, nil
			case none:
				return 0, nil
			}
			return media.Unlimited, nil
		},
	})
	ctx := context.Background()

	save := func(org uuid.UUID, n int) (media.File, error) {
		return s.Save(ctx, org, bytes.NewReader(sized(n)))
	}
	wantUsage := func(org uuid.UUID, bytes int64, files int) {
		t.Helper()
		got, err := s.Usage(ctx, org)
		if err != nil {
			t.Fatalf("Usage: %v", err)
		}
		if got.Bytes != bytes || got.Files != files {
			t.Errorf("Usage = %+v, ingin %d byte dalam %d berkas", got, bytes, files)
		}
	}

	wantUsage(limited, 0, 0)
	first, err := save(limited, 2<<10)
	if err != nil {
		t.Fatalf("Save di bawah kuota: %v", err)
	}
	wantUsage(limited, 2<<10, 1)

	// 2 KB terpakai dari 3 KB: 1,5 KB lagi tidak muat, dan tidak tersimpan.
	_, err = save(limited, 1<<10+512)
	wantQuotaExceeded(t, err, "2 KB dari 3 KB")
	wantUsage(limited, 2<<10, 1)

	// Tepat sampai batas masih boleh.
	if _, err := save(limited, 1<<10); err != nil {
		t.Fatalf("Save tepat sampai batas: %v", err)
	}
	_, err = save(limited, 64)
	wantQuotaExceeded(t, err, "3 KB dari 3 KB")

	// Kuota dihitung per organization.
	if _, err := save(unlimited, 8<<10); err != nil {
		t.Errorf("Save organization tanpa batas: %v", err)
	}
	wantUsage(unlimited, 8<<10, 1)

	// Batas nol bukan tanpa batas: hak pakai yang tidak dibawa paket dijawab
	// nol, dan itu berarti tidak boleh menyimpan sama sekali.
	_, err = save(none, 64)
	wantQuotaExceeded(t, err, "tidak menyertakan penyimpanan")
	wantUsage(none, 0, 0)

	// Menghapus berkas mengembalikan ruangnya.
	if err := s.Delete(ctx, limited, first.ID); err != nil {
		t.Fatal(err)
	}
	wantUsage(limited, 1<<10, 1)
	if _, err := save(limited, 1<<10+512); err != nil {
		t.Errorf("Save setelah ruang dikosongkan: %v", err)
	}
}

// Berkas yang ditolak karena hal lain tidak menyentuh kuota: pemeriksaan
// isian didahulukan, supaya pesannya tentang berkasnya.
func TestQuotaAfterValidation(t *testing.T) {
	calls := 0
	s := newService(t, media.Options{
		Quota: func(context.Context, uuid.UUID) (int64, error) {
			calls++
			return 1, nil
		},
	})
	_, err := s.Save(context.Background(), uuid.New(), strings.NewReader("bukan gambar"))
	wantValidation(t, err, "PNG, JPEG, atau WebP")
	if calls != 0 {
		t.Errorf("Quota dipanggil %d kali untuk berkas yang tidak sah", calls)
	}
}

// Kuota yang gagal dibaca menggagalkan unggahan: lebih baik menolak daripada
// menyimpan tanpa batas.
func TestQuotaLookupFails(t *testing.T) {
	down := errors.New("layanan hak pakai mati")
	s := newService(t, media.Options{
		Quota: func(context.Context, uuid.UUID) (int64, error) { return 0, down },
	})
	org := uuid.New()
	_, err := s.Save(context.Background(), org, bytes.NewReader(sized(64)))
	if !errors.Is(err, down) {
		t.Fatalf("galat = %v, ingin galat pembacaan kuota", err)
	}
	if _, ok := errors.AsType[*appkit.Error](err); ok {
		t.Error("galat pembacaan kuota tidak boleh menjadi pesan untuk pengguna")
	}
	if usage, _ := s.Usage(context.Background(), org); usage.Files != 0 {
		t.Errorf("berkas tersimpan walau kuota tidak terbaca: %+v", usage)
	}
}

// memStore adalah penyimpanan di memori, pengganti object storage di test.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = data
	return nil
}

func (m *memStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *memStore) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// Produk yang memasang penyimpanan lain tidak kehilangan berkas yang isinya
// sudah di database: berkas lama tetap terbaca dan tetap dapat dihapus,
// sedangkan berkas baru hanya masuk ke penyimpanan baru.
func TestStoreSwitchKeepsOldFiles(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	org := uuid.New()
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

	before, err := media.New(pool, testdb.Hooks(), media.Options{})
	if err != nil {
		t.Fatal(err)
	}
	oldData := pngBytes(t, 8, 8)
	old, err := before.Save(ctx, org, bytes.NewReader(oldData))
	if err != nil {
		t.Fatal(err)
	}

	mem := &memStore{objects: map[string][]byte{}}
	after, err := media.New(pool, testdb.Hooks(), media.Options{Store: mem})
	if err != nil {
		t.Fatal(err)
	}
	newData := pngBytes(t, 16, 16)
	fresh, err := after.Save(ctx, org, bytes.NewReader(newData))
	if err != nil {
		t.Fatal(err)
	}
	if blobs() != 1 || mem.len() != 1 {
		t.Fatalf("isi di database = %d, di penyimpanan baru = %d; ingin 1 dan 1", blobs(), mem.len())
	}

	if !bytes.Equal(read(after, old.ID), oldData) {
		t.Error("berkas lama tidak terbaca setelah penyimpanan diganti")
	}
	if !bytes.Equal(read(after, fresh.ID), newData) {
		t.Error("berkas baru tidak terbaca")
	}

	for _, id := range []uuid.UUID{old.ID, fresh.ID} {
		if err := after.Delete(ctx, org, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if blobs() != 0 || mem.len() != 0 {
		t.Errorf("sisa isi: database %d, penyimpanan baru %d", blobs(), mem.len())
	}
}

// Galat penyimpanan selain "tidak ada" tidak boleh ditutupi dengan mencari
// ke database: berkasnya akan tampak hilang, padahal penyimpanannya yang
// bermasalah.
func TestStoreFailureIsNotMissing(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	mem := &memStore{objects: map[string][]byte{}}
	s, err := media.New(pool, testdb.Hooks(), media.Options{Store: failingOpen{mem}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Save(ctx, uuid.New(), bytes.NewReader(pngBytes(t, 8, 8)))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Open(ctx, f.ID)
	if !errors.Is(err, errStoreDown) {
		t.Errorf("galat = %v, ingin galat penyimpanan", err)
	}
}

var errStoreDown = errors.New("penyimpanan tidak terjangkau")

type failingOpen struct{ media.Store }

func (failingOpen) Open(context.Context, string) (io.ReadCloser, error) { return nil, errStoreDown }

// Berkas pengganti tidak ikut menghitung berkas yang digantikannya: kuota
// yang penuh tidak mengunci organization dari mengganti gambarnya.
func TestSaveReplacing(t *testing.T) {
	s := newService(t, media.Options{
		Quota: func(context.Context, uuid.UUID) (int64, error) { return 2 << 10, nil },
	})
	ctx := context.Background()
	org, other := uuid.New(), uuid.New()

	old, err := s.Save(ctx, org, bytes.NewReader(sized(2<<10)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Save(ctx, org, bytes.NewReader(sized(1<<10)))
	wantQuotaExceeded(t, err, "2 KB dari 2 KB")

	if _, err := s.SaveReplacing(ctx, org, bytes.NewReader(sized(2<<10)), &old.ID); err != nil {
		t.Fatalf("pengganti seukuran: %v", err)
	}
	// Berkas lama belum dihapus pemanggil: pemakaian sementara di atas batas,
	// dan pengganti yang lebih besar dari sisa ruang tetap ditolak.
	_, err = s.SaveReplacing(ctx, org, bytes.NewReader(sized(1<<10)), &old.ID)
	wantQuotaExceeded(t, err, "4 KB dari 2 KB")

	// Menyebut berkas organization lain tidak menambah ruang.
	foreign, err := s.Save(ctx, other, bytes.NewReader(sized(2<<10)))
	if err != nil {
		t.Fatal(err)
	}
	fresh := uuid.New()
	if _, err := s.Save(ctx, fresh, bytes.NewReader(sized(2<<10))); err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveReplacing(ctx, fresh, bytes.NewReader(sized(1<<10)), &foreign.ID)
	wantQuotaExceeded(t, err, "2 KB dari 2 KB")

	// Tanpa yang digantikan, SaveReplacing sama dengan Save.
	if _, err := s.SaveReplacing(ctx, uuid.New(), bytes.NewReader(sized(1<<10)), nil); err != nil {
		t.Errorf("SaveReplacing tanpa berkas lama: %v", err)
	}
}
