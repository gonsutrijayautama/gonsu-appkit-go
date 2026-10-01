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
