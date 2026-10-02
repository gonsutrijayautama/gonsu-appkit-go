package attachments_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/attachments"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media/s3store"
)

// Kepala berkas yang cukup untuk dikenali; isinya tidak didekode.
var (
	pdfBytes  = []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
	webpBytes = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sized mengembalikan "PDF" berukuran tepat n byte: kepala yang cukup untuk
// dikenali, sisanya nol.
func sized(n int) []byte {
	data := make([]byte, n)
	copy(data, "%PDF-1.7\n")
	return data
}

// workOrder mengembalikan Owner sebuah dokumen baru.
func workOrder() attachments.Owner {
	return attachments.Owner{Type: "work_order", ID: uuid.NewString()}
}

func newService(t *testing.T, opts attachments.Options) *attachments.Service {
	t.Helper()
	return newServiceOn(t, testdb.New(t), opts)
}

func newServiceOn(t *testing.T, pool *pgxpool.Pool, opts attachments.Options) *attachments.Service {
	t.Helper()
	s, err := attachments.New(pool, opts)
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

func wantNotFound(t *testing.T, what string, err error) {
	t.Helper()
	e, ok := errors.AsType[*appkit.Error](err)
	if !ok || e.Kind != appkit.KindNotFound || e.Message != "Berkas tidak ditemukan." {
		t.Errorf("%s = %v, ingin tidak ditemukan", what, err)
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

// same membandingkan dua File: waktu dan pengunggah dibandingkan nilainya.
func same(a, b attachments.File) bool {
	uploader := func(f attachments.File) uuid.UUID {
		if f.UploadedBy == nil {
			return uuid.Nil
		}
		return *f.UploadedBy
	}
	return a.ID == b.ID && a.Owner == b.Owner && a.Filename == b.Filename && a.ContentType == b.ContentType &&
		a.Size == b.Size && uploader(a) == uploader(b) && a.CreatedAt.Equal(b.CreatedAt)
}

// blobKeys mengembalikan key seluruh isi berkas yang tersimpan di database.
func blobKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT key FROM appkit_media_blobs ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	return keys
}

func read(t *testing.T, s *attachments.Service, org uuid.UUID, owner attachments.Owner, id uuid.UUID) []byte {
	t.Helper()
	_, rc, err := s.Open(context.Background(), org, owner, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("membaca isi: %v", err)
	}
	return content
}

func TestNew(t *testing.T) {
	if _, err := attachments.New(nil, attachments.Options{}); err == nil {
		t.Error("New tanpa pool lolos")
	}

	pool := testdb.New(t)
	if s := newServiceOn(t, pool, attachments.Options{}); s.MaxBytes() != attachments.DefaultMaxBytes || s.MaxBytes() != 10<<20 {
		t.Errorf("MaxBytes bawaan = %d", s.MaxBytes())
	}
	if s := newServiceOn(t, pool, attachments.Options{MaxBytes: 1 << 10}); s.MaxBytes() != 1<<10 {
		t.Errorf("MaxBytes = %d, ingin yang diisi", s.MaxBytes())
	}

	// SVG dan HTML tidak pernah diterima, walau produk mendaftarkannya; salah
	// tulis jenis juga gagal saat start.
	for name, types := range map[string][]string{
		"svg":                 {"application/pdf", "image/svg+xml"},
		"html":                {"text/html"},
		"html berhuruf besar": {" Text/HTML "},
		"xhtml":               {"application/xhtml+xml"},
		"bukan jenis berkas":  {"pdf"},
		"berparameter":        {"text/plain; charset=utf-8"},
		"kosong":              {""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := attachments.New(pool, attachments.Options{Types: types}); err == nil {
				t.Errorf("New dengan Types %q lolos", types)
			}
		})
	}
}

func TestSaveListGetOpen(t *testing.T) {
	s := newService(t, attachments.Options{})
	ctx := context.Background()
	org, user := uuid.New(), uuid.New()
	owner := workOrder()

	var saved []attachments.File
	for _, tc := range []struct {
		filename    string
		data        []byte
		contentType string
	}{
		{"kontrak.pdf", pdfBytes, "application/pdf"},
		{"foto-qc.png", pngBytes(t), "image/png"},
		{"foto-qc.jpg", jpegBytes(t), "image/jpeg"},
		{"pola.webp", webpBytes, "image/webp"},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			f, err := s.Save(ctx, org, owner, tc.filename, user, bytes.NewReader(tc.data))
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			saved = append(saved, f)
			if f.ID == uuid.Nil || f.Owner != owner || f.Filename != tc.filename || f.ContentType != tc.contentType ||
				f.Size != int64(len(tc.data)) || f.UploadedBy == nil || *f.UploadedBy != user || f.CreatedAt.IsZero() {
				t.Errorf("berkas = %+v", f)
			}

			got, err := s.Get(ctx, org, owner, f.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !same(got, f) {
				t.Errorf("Get = %+v, ingin %+v", got, f)
			}

			opened, rc, err := s.Open(ctx, org, owner, f.ID)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer rc.Close()
			content, _ := io.ReadAll(rc)
			if !bytes.Equal(content, tc.data) || !same(opened, f) {
				t.Error("isi yang dibuka berbeda dari yang disimpan")
			}
		})
	}

	// Terlama dulu: urutan mengunggahnya.
	list, err := s.List(ctx, org, owner)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(saved) {
		t.Fatalf("List = %d berkas, ingin %d", len(list), len(saved))
	}
	for i := range list {
		if !same(list[i], saved[i]) {
			t.Errorf("List[%d] = %+v, ingin %+v", i, list[i], saved[i])
		}
	}

	// Dokumen tanpa lampiran dijawab daftar kosong, bukan nil: di JSON ia [].
	empty, err := s.List(ctx, org, workOrder())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("List dokumen tanpa lampiran = %v, %v; ingin daftar kosong", empty, err)
	}
	// Jenis dokumen ikut membedakan: id yang sama di jenis lain bukan dokumen
	// yang sama.
	if other, err := s.List(ctx, org, attachments.Owner{Type: "contract", ID: owner.ID}); err != nil || len(other) != 0 {
		t.Errorf("List jenis dokumen lain = %v, %v", other, err)
	}

	wantNotFound(t, "Get berkas yang tidak ada", func() error { _, err := s.Get(ctx, org, owner, uuid.New()); return err }())
	_, _, err = s.Open(ctx, org, owner, uuid.New())
	wantNotFound(t, "Open berkas yang tidak ada", err)
}

// Bentuk JSON adalah kontrak: nama field, dan TANPA url — berkas privat tidak
// punya alamat publik.
func TestFileJSON(t *testing.T) {
	s := newService(t, attachments.Options{})
	ctx := context.Background()
	org, user := uuid.New(), uuid.New()
	owner := workOrder()

	keys := func(f attachments.File) (map[string]any, []string) {
		t.Helper()
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out, slices.Sorted(maps.Keys(out))
	}

	byUser, err := s.Save(ctx, org, owner, "kontrak.pdf", user, bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}
	body, names := keys(byUser)
	want := []string{"content_type", "created_at", "filename", "id", "owner", "size", "uploaded_by"}
	if !slices.Equal(names, want) {
		t.Errorf("field = %v, ingin %v", names, want)
	}
	if o, _ := body["owner"].(map[string]any); len(o) != 2 || o["type"] != owner.Type || o["id"] != owner.ID {
		t.Errorf("owner = %v", body["owner"])
	}
	if body["uploaded_by"] != user.String() || body["filename"] != "kontrak.pdf" || body["content_type"] != "application/pdf" {
		t.Errorf("isi = %v", body)
	}

	// Tanpa pengunggah: bukan tindakan seorang pengguna, dan tersimpan NULL.
	bySystem, err := s.Save(ctx, org, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, org, owner, bySystem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bySystem.UploadedBy != nil || got.UploadedBy != nil {
		t.Errorf("pengunggah = %v dan %v, ingin kosong", bySystem.UploadedBy, got.UploadedBy)
	}
	if body, _ := keys(got); body["uploaded_by"] != nil {
		t.Errorf("uploaded_by = %v, ingin null", body["uploaded_by"])
	}
}

// Nama berkas hanya menjadi nama unduhan: path-nya dibuang, dan yang tersisa
// selalu dapat disimpan.
func TestFilename(t *testing.T) {
	s := newService(t, attachments.Options{})
	ctx := context.Background()
	org := uuid.New()
	owner := workOrder()

	long := strings.Repeat("é", 300)
	for name, tc := range map[string]struct{ in, want string }{
		"biasa":              {"laporan akhir.pdf", "laporan akhir.pdf"},
		"path unix":          {"../../etc/passwd", "passwd"},
		"path windows":       {`C:\Users\budi\Dokumen\kontrak.pdf`, "kontrak.pdf"},
		"path campuran":      {`dir/sub\kontrak.pdf`, "kontrak.pdf"},
		"spasi di tepi":      {"  kontrak.pdf \t", "kontrak.pdf"},
		"karakter kendali":   {"kon\x00trak\r\n\x1b.pdf\x7f", "kontrak.pdf"},
		"kutip tetap":        {`kontrak "final".pdf`, `kontrak "final".pdf`},
		"bukan ascii":        {"surat perintah – 日本.pdf", "surat perintah – 日本.pdf"},
		"bukan utf-8":        {"kon\xfftrak.pdf", "kontrak.pdf"},
		"kosong":             {"", "berkas"},
		"hanya spasi":        {" \t ", "berkas"},
		"hanya path":         {"dir/sub/", "berkas"},
		"titik":              {".", "berkas"},
		"titik dua":          {"dir/..", "berkas"},
		"hanya kendali":      {"\x00\n", "berkas"},
		"terlalu panjang":    {long + ".pdf", strings.Repeat("é", 200)},
		"dipotong lalu trim": {strings.Repeat("a", 199) + " b.pdf", strings.Repeat("a", 199)},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := s.Save(ctx, org, owner, tc.in, uuid.Nil, bytes.NewReader(pdfBytes))
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := s.Get(ctx, org, owner, f.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if f.Filename != tc.want || got.Filename != tc.want {
				t.Errorf("nama = %q, tersimpan %q; ingin %q", f.Filename, got.Filename, tc.want)
			}
			if n := utf8.RuneCountInString(got.Filename); n > 200 {
				t.Errorf("panjang nama = %d karakter", n)
			}
		})
	}
}

// Jenis berkas dikenali dari isinya: nama berkas tidak menentukan apa pun.
func TestTypeFromContent(t *testing.T) {
	s := newService(t, attachments.Options{MaxBytes: 4 << 10})
	ctx := context.Background()
	org := uuid.New()
	owner := workOrder()

	t.Run("pdf bernama png disimpan sebagai pdf", func(t *testing.T) {
		f, err := s.Save(ctx, org, owner, "foto.png", uuid.Nil, bytes.NewReader(pdfBytes))
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := s.Get(ctx, org, owner, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if f.ContentType != "application/pdf" || got.ContentType != "application/pdf" || got.Filename != "foto.png" {
			t.Errorf("berkas = %+v", got)
		}
	})

	for name, tc := range map[string]struct{ filename, data string }{
		"html bernama pdf":      {"kontrak.pdf", `<!doctype html><html><body><script>alert(1)</script></body></html>`},
		"html tanpa doctype":    {"kontrak.pdf", `<html><script>alert(1)</script></html>`},
		"svg":                   {"gambar.png", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		"svg dengan prolog xml": {"gambar.png", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		"teks":                  {"catatan.pdf", "bukan berkas yang diterima"},
		"zip":                   {"arsip.pdf", "PK\x03\x04" + strings.Repeat("\x00", 32)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Save(ctx, org, owner, tc.filename, uuid.Nil, strings.NewReader(tc.data))
			wantValidation(t, err, "Yang diterima: PDF, PNG, JPEG, WebP.")
		})
	}

	t.Run("kosong", func(t *testing.T) {
		_, err := s.Save(ctx, org, owner, "kosong.pdf", uuid.Nil, strings.NewReader(""))
		wantValidation(t, err, "kosong")
	})
	t.Run("terlalu besar", func(t *testing.T) {
		_, err := s.Save(ctx, org, owner, "besar.pdf", uuid.Nil, bytes.NewReader(sized(4<<10+1)))
		wantValidation(t, err, "maksimal 4 KB")
	})
	t.Run("tepat sebesar batas", func(t *testing.T) {
		if _, err := s.Save(ctx, org, owner, "pas.pdf", uuid.Nil, bytes.NewReader(sized(4<<10))); err != nil {
			t.Errorf("Save: %v", err)
		}
	})
	// Body yang sudah dibatasi pemanggil dengan http.MaxBytesReader dijawab
	// dengan pesan yang sama, bukan galat tak terduga.
	t.Run("body melewati batas pemanggil", func(t *testing.T) {
		body := http.MaxBytesReader(nil, io.NopCloser(bytes.NewReader(sized(2<<10))), 1<<10)
		_, err := s.Save(ctx, org, owner, "besar.pdf", uuid.Nil, body)
		wantValidation(t, err, "maksimal 4 KB")
	})
	t.Run("tanpa organization", func(t *testing.T) {
		if _, err := s.Save(ctx, uuid.Nil, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes)); err == nil {
			t.Error("Save tanpa organization lolos")
		}
	})

	// Tidak satu pun dari yang ditolak meninggalkan baris; yang tersimpan
	// hanya dua berkas yang sah di atas.
	if usage, err := s.Usage(ctx, org); err != nil || usage.Files != 2 {
		t.Errorf("Usage = %+v, %v; ingin 2 berkas", usage, err)
	}
}

// Jenis yang diterima diatur produk, dan tetap dikenali dari isinya.
func TestTypesOption(t *testing.T) {
	s := newService(t, attachments.Options{Types: []string{"Text/Plain", "application/zip", "text/plain"}})
	ctx := context.Background()
	org := uuid.New()
	owner := workOrder()

	// Parameter jenisnya tidak disimpan.
	f, err := s.Save(ctx, org, owner, "catatan.txt", uuid.Nil, strings.NewReader("catatan pemeriksaan"))
	if err != nil {
		t.Fatalf("Save teks: %v", err)
	}
	if f.ContentType != "text/plain" {
		t.Errorf("jenis = %q, ingin text/plain", f.ContentType)
	}
	// Berkas .docx dikenali sebagai arsip zip.
	f, err = s.Save(ctx, org, owner, "surat.docx", uuid.Nil, strings.NewReader("PK\x03\x04"+strings.Repeat("\x00", 32)))
	if err != nil || f.ContentType != "application/zip" {
		t.Errorf("Save zip = %+v, %v", f, err)
	}

	// Yang tidak didaftarkan ditolak, termasuk jenis bawaan; pesannya
	// menyebut yang diterima.
	_, err = s.Save(ctx, org, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
	wantValidation(t, err, "Yang diterima: text/plain, application/zip.")
	// HTML tetap dikenali sebagai HTML, dan ditolak.
	_, err = s.Save(ctx, org, owner, "catatan.txt", uuid.Nil, strings.NewReader("<html><script>alert(1)</script></html>"))
	wantValidation(t, err, "tidak diterima")

	// SVG tanpa prolog XML tidak dikenali sebagai apa pun selain teks. Bila
	// produk menerima teks, ia tersimpan SEBAGAI teks — tidak pernah sebagai
	// SVG — dan Serve menyajikannya sebagai unduhan teks.
	f, err = s.Save(ctx, org, owner, "gambar.svg", uuid.Nil, strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	if err != nil || f.ContentType != "text/plain" {
		t.Errorf("Save svg sebagai teks = %+v, %v", f, err)
	}
}

// Owner diisi kode modul pemanggil. Yang tidak sah ditolak sebelum menyentuh
// database, dengan galat yang bukan untuk pengguna.
func TestOwnerValidation(t *testing.T) {
	s := newService(t, attachments.Options{})
	ctx := context.Background()
	org := uuid.New()

	for name, owner := range map[string]attachments.Owner{
		"jenis kosong":           {Type: "", ID: "1"},
		"jenis berhuruf besar":   {Type: "WorkOrder", ID: "1"},
		"jenis diawali angka":    {Type: "1order", ID: "1"},
		"jenis dengan tanda":     {Type: "work-order", ID: "1"},
		"jenis terlalu panjang":  {Type: strings.Repeat("a", 61), ID: "1"},
		"jenis dengan baris":     {Type: "work_order\n", ID: "1"},
		"id kosong":              {Type: "work_order", ID: ""},
		"id terlalu panjang":     {Type: "work_order", ID: strings.Repeat("é", 201)},
		"id bukan utf-8":         {Type: "work_order", ID: "a\xffb"},
		"id berkarakter kendali": {Type: "work_order", ID: "a\x00b"},
	} {
		t.Run(name, func(t *testing.T) {
			check := func(what string, err error) {
				t.Helper()
				if err == nil {
					t.Errorf("%s dengan owner %+v lolos", what, owner)
				}
				if _, ok := errors.AsType[*appkit.Error](err); ok {
					t.Errorf("%s: galat owner tidak boleh menjadi pesan untuk pengguna: %v", what, err)
				}
			}
			_, err := s.Save(ctx, org, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
			check("Save", err)
			_, err = s.List(ctx, org, owner)
			check("List", err)
			_, err = s.DeleteOwner(ctx, org, owner)
			check("DeleteOwner", err)
			_, err = s.Get(ctx, org, owner, uuid.New())
			check("Get", err)
			_, _, err = s.Open(ctx, org, owner, uuid.New())
			check("Open", err)
			check("Delete", s.Delete(ctx, org, owner, uuid.New()))
		})
	}

	// Batas terpanjang yang masih sah.
	longest := attachments.Owner{Type: "a" + strings.Repeat("9", 59), ID: strings.Repeat("é", 200)}
	if _, err := s.Save(ctx, org, longest, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes)); err != nil {
		t.Errorf("Save dengan owner terpanjang: %v", err)
	}
	if list, err := s.List(ctx, org, longest); err != nil || len(list) != 1 {
		t.Errorf("List owner terpanjang = %v, %v", list, err)
	}
}

// CHECK di tabel menjaga hal yang sama dengan kode: baris yang tidak sah
// ditolak database walau ditulis di luar Service.
func TestTableConstraints(t *testing.T) {
	pool := testdb.New(t)
	insert := func(ownerType, ownerID, filename, contentType string, size int64) error {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO appkit_attachments
				(id, organization_id, owner_type, owner_id, filename, content_type, size_bytes, sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			uuid.New(), uuid.New(), ownerType, ownerID, filename, contentType, size, strings.Repeat("a", 64))
		return err
	}

	for name, err := range map[string]error{
		"pdf":              insert("work_order", "1", "kontrak.pdf", "application/pdf", 1),
		"batas terpanjang": insert("a"+strings.Repeat("9", 59), strings.Repeat("é", 200), strings.Repeat("é", 200), "application/vnd.ms-fontobject", 1),
		"jenis bertanda":   insert("work_order", "1", "arsip.gz", "application/x-gzip", 1),
	} {
		if err != nil {
			t.Errorf("baris sah %s ditolak: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"svg":                         insert("work_order", "1", "gambar.svg", "image/svg+xml", 1),
		"html":                        insert("work_order", "1", "halaman.html", "text/html", 1),
		"xhtml":                       insert("work_order", "1", "halaman.xhtml", "application/xhtml+xml", 1),
		"jenis berparameter":          insert("work_order", "1", "catatan.txt", "text/plain; charset=utf-8", 1),
		"jenis berhuruf besar":        insert("work_order", "1", "kontrak.pdf", "Application/PDF", 1),
		"jenis tanpa garis miring":    insert("work_order", "1", "kontrak.pdf", "pdf", 1),
		"jenis dokumen kosong":        insert("", "1", "kontrak.pdf", "application/pdf", 1),
		"jenis dokumen huruf besar":   insert("WorkOrder", "1", "kontrak.pdf", "application/pdf", 1),
		"jenis dokumen kepanjangan":   insert(strings.Repeat("a", 61), "1", "kontrak.pdf", "application/pdf", 1),
		"id dokumen kosong":           insert("work_order", "", "kontrak.pdf", "application/pdf", 1),
		"id dokumen kepanjangan":      insert("work_order", strings.Repeat("a", 201), "kontrak.pdf", "application/pdf", 1),
		"nama berkas kosong":          insert("work_order", "1", "", "application/pdf", 1),
		"nama berkas kepanjangan":     insert("work_order", "1", strings.Repeat("a", 201), "application/pdf", 1),
		"ukuran nol":                  insert("work_order", "1", "kontrak.pdf", "application/pdf", 0),
		"jenis berkas terlalu banyak": insert("work_order", "1", "kontrak.pdf", "application/"+strings.Repeat("a", 100), 1),
	} {
		if e, ok := errors.AsType[*pgconn.PgError](err); !ok || e.Code != "23514" {
			t.Errorf("baris %s = %v, ingin ditolak CHECK", name, err)
		}
	}
}

// Setiap jalur baca dan hapus menyaring organization: organization lain tidak
// melihat apa pun dan tidak mengubah apa pun. Tidak ada jalur baca publik.
func TestTenantIsolation(t *testing.T) {
	pool := testdb.New(t)
	s := newServiceOn(t, pool, attachments.Options{})
	ctx := context.Background()
	mine, other := uuid.New(), uuid.New()
	owner := workOrder()

	f, err := s.Save(ctx, mine, owner, "kontrak.pdf", uuid.New(), bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}
	// Organization lain punya dokumen dengan sebutan yang SAMA: id dokumen
	// produk tidak dijamin unik lintas organization.
	theirs, err := s.Save(ctx, other, owner, "milik-lain.pdf", uuid.New(), bytes.NewReader(sized(64)))
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.Get(ctx, other, owner, f.ID)
	wantNotFound(t, "Get lintas organization", err)
	_, rc, err := s.Open(ctx, other, owner, f.ID)
	wantNotFound(t, "Open lintas organization", err)
	if rc != nil {
		t.Error("Open lintas organization mengembalikan isi")
	}
	_, _, err = s.Open(ctx, uuid.Nil, owner, f.ID)
	wantNotFound(t, "Open tanpa organization", err)

	list, err := s.List(ctx, other, owner)
	if err != nil || len(list) != 1 || list[0].ID != theirs.ID {
		t.Errorf("List organization lain = %+v, %v; ingin hanya berkasnya sendiri", list, err)
	}
	if list, err := s.List(ctx, uuid.New(), owner); err != nil || len(list) != 0 {
		t.Errorf("List organization ketiga = %+v, %v; ingin kosong", list, err)
	}

	if err := s.Delete(ctx, other, owner, f.ID); err != nil {
		t.Fatalf("Delete lintas organization: %v", err)
	}
	if n, err := s.DeleteOwner(ctx, uuid.New(), owner); err != nil || n != 0 {
		t.Errorf("DeleteOwner organization ketiga = %d, %v; ingin 0", n, err)
	}
	// DeleteOwner organization lain hanya menghapus berkasnya sendiri.
	if n, err := s.DeleteOwner(ctx, other, owner); err != nil || n != 1 {
		t.Errorf("DeleteOwner organization lain = %d, %v; ingin 1", n, err)
	}

	// Berkas pemiliknya utuh: baris, isi, dan pemakaiannya.
	got, err := s.Get(ctx, mine, owner, f.ID)
	if err != nil || !same(got, f) {
		t.Fatalf("berkas berubah oleh organization lain: %+v, %v", got, err)
	}
	if !bytes.Equal(read(t, s, mine, owner, f.ID), pdfBytes) {
		t.Error("isi berkas berubah oleh organization lain")
	}
	if list, err := s.List(ctx, mine, owner); err != nil || len(list) != 1 || list[0].ID != f.ID {
		t.Errorf("List pemilik = %+v, %v", list, err)
	}
	for org, want := range map[uuid.UUID]media.Usage{
		mine:  {Bytes: int64(len(pdfBytes)), Files: 1},
		other: {},
	} {
		if usage, err := s.Usage(ctx, org); err != nil || usage != want {
			t.Errorf("Usage = %+v, %v; ingin %+v", usage, err, want)
		}
	}
	if keys := blobKeys(t, pool); !slices.Equal(keys, []string{"private/" + mine.String() + "/" + f.ID.String()}) {
		t.Errorf("isi tersimpan = %v, ingin hanya milik pemiliknya", keys)
	}
}

// Sebuah berkas hanya dapat dibaca dan dihapus lewat dokumen yang
// dilampirinya. Izin atas satu dokumen tidak membuka lampiran dokumen lain di
// organization yang SAMA: jawabannya sama persis dengan berkas yang tidak ada,
// dan tidak ada yang terhapus.
func TestOwnerIsolation(t *testing.T) {
	pool := testdb.New(t)
	s := newServiceOn(t, pool, attachments.Options{})
	ctx := context.Background()
	org := uuid.New()
	owner, otherDocument := workOrder(), workOrder()

	f, err := s.Save(ctx, org, owner, "kontrak.pdf", uuid.New(), bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.Save(ctx, org, otherDocument, "dokumen-lain.pdf", uuid.New(), bytes.NewReader(sized(64)))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"private/" + org.String() + "/" + f.ID.String(), "private/" + org.String() + "/" + theirs.ID.String()}
	slices.Sort(keys)

	for name, wrong := range map[string]attachments.Owner{
		"dokumen lain yang punya lampiran": otherDocument,
		"dokumen tanpa lampiran":           workOrder(),
		"jenis lain ber-id sama":           {Type: "contract", ID: owner.ID},
		"id berawalan sama":                {Type: owner.Type, ID: owner.ID + "0"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Get(ctx, org, wrong, f.ID)
			wantNotFound(t, "Get lewat dokumen lain", err)
			got, rc, err := s.Open(ctx, org, wrong, f.ID)
			wantNotFound(t, "Open lewat dokumen lain", err)
			if rc != nil || got.ID != uuid.Nil {
				t.Errorf("Open lewat dokumen lain mengembalikan berkas %+v", got)
			}
			if err := s.Delete(ctx, org, wrong, f.ID); err != nil {
				t.Fatalf("Delete lewat dokumen lain: %v", err)
			}

			// Baris dan isinya utuh.
			kept, err := s.Get(ctx, org, owner, f.ID)
			if err != nil || !same(kept, f) {
				t.Fatalf("berkas berubah lewat dokumen lain: %+v, %v", kept, err)
			}
			if !bytes.Equal(read(t, s, org, owner, f.ID), pdfBytes) {
				t.Error("isi berkas berubah lewat dokumen lain")
			}
			if stored := blobKeys(t, pool); !slices.Equal(stored, keys) {
				t.Errorf("isi tersimpan = %v, ingin %v", stored, keys)
			}
		})
	}

	// Lampiran dokumen lain itu sendiri tidak tersentuh, dan tetap hanya
	// terbuka lewat dokumennya.
	if list, err := s.List(ctx, org, otherDocument); err != nil || len(list) != 1 || !same(list[0], theirs) {
		t.Errorf("List dokumen lain = %+v, %v", list, err)
	}
	_, err = s.Get(ctx, org, owner, theirs.ID)
	wantNotFound(t, "Get lampiran dokumen lain", err)
	if usage, err := s.Usage(ctx, org); err != nil || usage.Files != 2 {
		t.Errorf("Usage = %+v, %v; ingin 2 berkas", usage, err)
	}

	// Lewat dokumennya sendiri, berkasnya terhapus.
	if err := s.Delete(ctx, org, owner, f.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Get(ctx, org, owner, f.ID)
	wantNotFound(t, "Get setelah Delete", err)
	if stored := blobKeys(t, pool); !slices.Equal(stored, []string{"private/" + org.String() + "/" + theirs.ID.String()}) {
		t.Errorf("isi tersimpan setelah Delete = %v", stored)
	}
}

func TestDelete(t *testing.T) {
	pool := testdb.New(t)
	s := newServiceOn(t, pool, attachments.Options{})
	ctx := context.Background()
	org := uuid.New()
	owner := workOrder()

	f, err := s.Save(ctx, org, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.Save(ctx, org, owner, "foto.png", uuid.Nil, bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatal(err)
	}
	if keys := blobKeys(t, pool); len(keys) != 2 {
		t.Fatalf("isi tersimpan = %v, ingin 2", keys)
	}

	if err := s.Delete(ctx, org, owner, f.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Get(ctx, org, owner, f.ID)
	wantNotFound(t, "Get setelah Delete", err)
	_, _, err = s.Open(ctx, org, owner, f.ID)
	wantNotFound(t, "Open setelah Delete", err)
	// Baris dan isinya sama-sama terhapus; berkas lain tidak tersentuh.
	if keys := blobKeys(t, pool); !slices.Equal(keys, []string{"private/" + org.String() + "/" + kept.ID.String()}) {
		t.Errorf("isi tersimpan setelah Delete = %v", keys)
	}
	if list, err := s.List(ctx, org, owner); err != nil || len(list) != 1 || list[0].ID != kept.ID {
		t.Errorf("List setelah Delete = %+v, %v", list, err)
	}

	// Menghapus yang sudah tidak ada bukan galat.
	if err := s.Delete(ctx, org, owner, f.ID); err != nil {
		t.Errorf("Delete kedua: %v", err)
	}
	if err := s.Delete(ctx, org, owner, uuid.New()); err != nil {
		t.Errorf("Delete berkas yang tidak pernah ada: %v", err)
	}
}

func TestDeleteOwner(t *testing.T) {
	pool := testdb.New(t)
	s := newServiceOn(t, pool, attachments.Options{})
	ctx := context.Background()
	org := uuid.New()
	owner := workOrder()

	save := func(o attachments.Owner) attachments.File {
		t.Helper()
		f, err := s.Save(ctx, org, o, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	for range 3 {
		save(owner)
	}
	// Dokumen lain, dan dokumen ber-id sama di jenis lain, tidak ikut.
	sibling := save(workOrder())
	otherType := save(attachments.Owner{Type: "contract", ID: owner.ID})

	n, err := s.DeleteOwner(ctx, org, owner)
	if err != nil || n != 3 {
		t.Fatalf("DeleteOwner = %d, %v; ingin 3", n, err)
	}
	if list, err := s.List(ctx, org, owner); err != nil || len(list) != 0 {
		t.Errorf("List setelah DeleteOwner = %+v, %v", list, err)
	}
	want := []string{"private/" + org.String() + "/" + sibling.ID.String(), "private/" + org.String() + "/" + otherType.ID.String()}
	slices.Sort(want)
	if keys := blobKeys(t, pool); !slices.Equal(keys, want) {
		t.Errorf("isi tersimpan setelah DeleteOwner = %v, ingin %v", keys, want)
	}
	for _, f := range []attachments.File{sibling, otherType} {
		if !bytes.Equal(read(t, s, org, f.Owner, f.ID), pdfBytes) {
			t.Errorf("berkas dokumen lain %s ikut terhapus", f.ID)
		}
	}

	// Dokumen tanpa lampiran bukan galat.
	if n, err := s.DeleteOwner(ctx, org, owner); err != nil || n != 0 {
		t.Errorf("DeleteOwner kedua = %d, %v; ingin 0", n, err)
	}
}

func TestQuota(t *testing.T) {
	limited, unlimited, none := uuid.New(), uuid.New(), uuid.New()
	s := newService(t, attachments.Options{
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
	owner := workOrder()

	save := func(org uuid.UUID, n int) (attachments.File, error) {
		return s.Save(ctx, org, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(sized(n)))
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
	wantQuotaExceeded(t, err, "Penyimpanan penuh: 2 KB dari 3 KB sudah terpakai.")
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
	if err := s.Delete(ctx, limited, owner, first.ID); err != nil {
		t.Fatal(err)
	}
	wantUsage(limited, 1<<10, 1)
	if _, err := save(limited, 1<<10+512); err != nil {
		t.Errorf("Save setelah ruang dikosongkan: %v", err)
	}
}

// Kuota penyimpanan satu untuk media dan lampiran, ke DUA arah: masing-masing
// menghitung pemakaian yang lain lewat OtherUsage, dipasang dengan closure
// seperti di produk. Usage tiap package tetap hanya berkasnya sendiri.
func TestQuotaCountsOtherUsage(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	org := uuid.New()
	owner := workOrder()
	quota := func(context.Context, uuid.UUID) (int64, error) { return 4 << 10, nil }

	var files *attachments.Service
	logos, err := media.New(pool, testdb.Hooks(), media.Options{
		Quota: quota,
		OtherUsage: func(ctx context.Context, org uuid.UUID) (int64, error) {
			u, err := files.Usage(ctx, org)
			return u.Bytes, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	files = newServiceOn(t, pool, attachments.Options{
		Quota: quota,
		OtherUsage: func(ctx context.Context, org uuid.UUID) (int64, error) {
			u, err := logos.Usage(ctx, org)
			return u.Bytes, err
		},
	})

	saveLogo := func(org uuid.UUID, n int) (media.File, error) {
		webp := make([]byte, n)
		copy(webp, "RIFF\x24\x00\x00\x00WEBPVP8 ")
		return logos.Save(ctx, org, bytes.NewReader(webp))
	}
	saveFile := func(org uuid.UUID, n int) (attachments.File, error) {
		return files.Save(ctx, org, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(sized(n)))
	}
	wantUsage := func(mediaBytes, fileBytes int64) {
		t.Helper()
		m, err := logos.Usage(ctx, org)
		if err != nil {
			t.Fatalf("Usage media: %v", err)
		}
		f, err := files.Usage(ctx, org)
		if err != nil {
			t.Fatalf("Usage lampiran: %v", err)
		}
		if m.Bytes != mediaBytes || f.Bytes != fileBytes {
			t.Errorf("Usage = media %d byte, lampiran %d byte; ingin %d dan %d", m.Bytes, f.Bytes, mediaBytes, fileBytes)
		}
	}

	logo, err := saveLogo(org, 2<<10)
	if err != nil {
		t.Fatalf("Save media: %v", err)
	}
	if _, err := saveFile(org, 1<<10); err != nil {
		t.Fatalf("Save di bawah kuota bersama: %v", err)
	}
	wantUsage(2<<10, 1<<10)

	// 2 KB media + 1 KB lampiran dari 4 KB: 1,5 KB lagi tidak muat, di package
	// mana pun, dan pesannya menyebut pemakaian gabungan.
	_, err = saveFile(org, 1<<10+512)
	wantQuotaExceeded(t, err, "Penyimpanan penuh: 3 KB dari 4 KB sudah terpakai.")
	_, err = saveLogo(org, 1<<10+512)
	wantQuotaExceeded(t, err, "Penyimpanan penuh: 3 KB dari 4 KB sudah terpakai.")
	wantUsage(2<<10, 1<<10)

	// Tepat sampai batas masih boleh; sesudahnya keduanya penuh.
	if _, err := saveLogo(org, 1<<10); err != nil {
		t.Fatalf("Save media tepat sampai batas: %v", err)
	}
	_, err = saveFile(org, 64)
	wantQuotaExceeded(t, err, "4 KB dari 4 KB")
	_, err = saveLogo(org, 64)
	wantQuotaExceeded(t, err, "4 KB dari 4 KB")

	// Organization lain tidak ikut menanggung pemakaian itu.
	other := uuid.New()
	if _, err := saveFile(other, 2<<10); err != nil {
		t.Errorf("Save lampiran organization lain: %v", err)
	}
	if _, err := saveLogo(other, 2<<10); err != nil {
		t.Errorf("Save media organization lain: %v", err)
	}

	// Ruang yang dikosongkan di media terpakai lampiran, dan sebaliknya.
	if err := logos.Delete(ctx, org, logo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := saveFile(org, 2<<10); err != nil {
		t.Errorf("Save lampiran setelah media dihapus: %v", err)
	}
	wantUsage(1<<10, 3<<10)
	_, err = saveLogo(org, 1<<10)
	wantQuotaExceeded(t, err, "4 KB dari 4 KB")
	if n, err := files.DeleteOwner(ctx, org, owner); err != nil || n != 2 {
		t.Fatalf("DeleteOwner = %d, %v; ingin 2", n, err)
	}
	if _, err := saveLogo(org, 3<<10); err != nil {
		t.Errorf("Save media setelah lampiran dihapus: %v", err)
	}
	wantUsage(4<<10, 0)
}

// Berkas yang ditolak karena hal lain tidak menyentuh kuota: pemeriksaan
// isian didahulukan, supaya pesannya tentang berkasnya.
func TestQuotaAfterValidation(t *testing.T) {
	calls := 0
	s := newService(t, attachments.Options{
		Quota: func(context.Context, uuid.UUID) (int64, error) {
			calls++
			return 1, nil
		},
	})
	_, err := s.Save(context.Background(), uuid.New(), workOrder(), "catatan.pdf", uuid.Nil, strings.NewReader("bukan berkas yang diterima"))
	wantValidation(t, err, "tidak diterima")
	if calls != 0 {
		t.Errorf("Quota dipanggil %d kali untuk berkas yang tidak sah", calls)
	}
}

// Kuota atau pemakaian lain yang gagal dibaca menggagalkan unggahan: lebih
// baik menolak daripada menyimpan tanpa batas.
func TestQuotaLookupFails(t *testing.T) {
	down := errors.New("layanan hak pakai mati")
	for name, opts := range map[string]attachments.Options{
		"kuota": {
			Quota: func(context.Context, uuid.UUID) (int64, error) { return 0, down },
		},
		"pemakaian lain": {
			Quota:      func(context.Context, uuid.UUID) (int64, error) { return 1 << 20, nil },
			OtherUsage: func(context.Context, uuid.UUID) (int64, error) { return 0, down },
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(t, opts)
			org := uuid.New()
			_, err := s.Save(context.Background(), org, workOrder(), "kontrak.pdf", uuid.Nil, bytes.NewReader(sized(64)))
			if !errors.Is(err, down) {
				t.Fatalf("galat = %v, ingin galat pembacaan", err)
			}
			if _, ok := errors.AsType[*appkit.Error](err); ok {
				t.Error("galat pembacaan kuota tidak boleh menjadi pesan untuk pengguna")
			}
			if usage, _ := s.Usage(context.Background(), org); usage.Files != 0 {
				t.Errorf("berkas tersimpan walau kuota tidak terbaca: %+v", usage)
			}
		})
	}

	// Tanpa batas, pemakaian lain tidak perlu dibaca — dan tidak dibaca.
	for name, quota := range map[string]func(context.Context, uuid.UUID) (int64, error){
		"tanpa Quota": nil,
		"tanpa batas": func(context.Context, uuid.UUID) (int64, error) { return media.Unlimited, nil },
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(t, attachments.Options{
				Quota:      quota,
				OtherUsage: func(context.Context, uuid.UUID) (int64, error) { return 0, down },
			})
			if _, err := s.Save(context.Background(), uuid.New(), workOrder(), "kontrak.pdf", uuid.Nil, bytes.NewReader(sized(64))); err != nil {
				t.Errorf("Save = %v; OtherUsage tidak boleh dibaca tanpa batas", err)
			}
		})
	}
}

// memStore adalah penyimpanan di memori, pengganti object storage di test.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

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

func (m *memStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.objects))
}

var errStoreDown = errors.New("penyimpanan tidak terjangkau")

type failingPut struct{ media.Store }

func (failingPut) Put(context.Context, string, io.Reader, int64, string) error { return errStoreDown }

type failingOpen struct{ media.Store }

func (failingOpen) Open(context.Context, string) (io.ReadCloser, error) { return nil, errStoreDown }

// cancelAfterPut membatalkan request tepat setelah isinya tersimpan, sehingga
// pencatatan barisnya gagal.
type cancelAfterPut struct {
	media.Store
	cancel context.CancelFunc
}

func (c cancelAfterPut) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	err := c.Store.Put(ctx, key, r, size, contentType)
	c.cancel()
	return err
}

// Delete menolak request yang sudah dibatalkan, seperti penyimpanan sungguhan.
func (c cancelAfterPut) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Store.Delete(ctx, key)
}

// Isi yang gagal disimpan tidak meninggalkan baris: lampiran yang tercatat
// selalu dapat dibuka.
func TestPutFailureLeavesNoRow(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	mem := newMemStore()
	s := newServiceOn(t, pool, attachments.Options{Store: failingPut{mem}})
	org := uuid.New()
	owner := workOrder()

	_, err := s.Save(ctx, org, owner, "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("galat = %v, ingin galat penyimpanan", err)
	}
	if _, ok := errors.AsType[*appkit.Error](err); ok {
		t.Error("galat penyimpanan tidak boleh menjadi pesan untuk pengguna")
	}
	if usage, err := s.Usage(ctx, org); err != nil || usage.Files != 0 {
		t.Errorf("Usage = %+v, %v; ingin tanpa berkas", usage, err)
	}
	if list, err := s.List(ctx, org, owner); err != nil || len(list) != 0 {
		t.Errorf("List = %+v, %v; ingin kosong", list, err)
	}
	if len(mem.keys()) != 0 || len(blobKeys(t, pool)) != 0 {
		t.Errorf("isi tersimpan: penyimpanan %v, database %v", mem.keys(), blobKeys(t, pool))
	}
}

// Baris yang gagal dicatat tidak meninggalkan isi: isinya dihapus lagi, walau
// request-nya sendiri sudah dibatalkan.
func TestRowFailureRemovesContent(t *testing.T) {
	pool := testdb.New(t)
	mem := newMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newServiceOn(t, pool, attachments.Options{Store: cancelAfterPut{mem, cancel}})
	org := uuid.New()

	_, err := s.Save(ctx, org, workOrder(), "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("galat = %v, ingin request yang dibatalkan", err)
	}
	if keys := mem.keys(); len(keys) != 0 {
		t.Errorf("isi tertinggal tanpa baris: %v", keys)
	}
	if usage, err := s.Usage(context.Background(), org); err != nil || usage.Files != 0 {
		t.Errorf("Usage = %+v, %v; ingin tanpa berkas", usage, err)
	}
}

// Produk yang memasang penyimpanan lain tidak kehilangan lampiran yang isinya
// sudah di database: berkas lama tetap terbaca dan tetap dapat dihapus,
// sedangkan berkas baru hanya masuk ke penyimpanan baru. Key-nya berawalan
// "private/", jadi penyimpanan yang sama dapat dipakai bersama media.
func TestStoreSwitchKeepsOldFiles(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	org := uuid.New()
	owner := workOrder()

	before := newServiceOn(t, pool, attachments.Options{})
	old, err := before.Save(ctx, org, owner, "lama.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}

	mem := newMemStore()
	after := newServiceOn(t, pool, attachments.Options{Store: mem})
	newData := pngBytes(t)
	fresh, err := after.Save(ctx, org, owner, "baru.png", uuid.Nil, bytes.NewReader(newData))
	if err != nil {
		t.Fatal(err)
	}
	oldKey := "private/" + org.String() + "/" + old.ID.String()
	freshKey := "private/" + org.String() + "/" + fresh.ID.String()
	if db, store := blobKeys(t, pool), mem.keys(); !slices.Equal(db, []string{oldKey}) || !slices.Equal(store, []string{freshKey}) {
		t.Fatalf("isi di database = %v, di penyimpanan baru = %v", db, store)
	}

	// Media di penyimpanan yang sama: key-nya tidak berawalan "private/", jadi
	// keduanya tidak pernah saling menimpa.
	logos, err := media.New(pool, testdb.Hooks(), media.Options{Store: mem})
	if err != nil {
		t.Fatal(err)
	}
	logo, err := logos.Save(ctx, org, bytes.NewReader(webpBytes))
	if err != nil {
		t.Fatal(err)
	}
	logoKey := org.String() + "/" + logo.ID.String()
	if keys := mem.keys(); !slices.Contains(keys, logoKey) || !slices.Contains(keys, freshKey) || len(keys) != 2 {
		t.Errorf("isi di penyimpanan bersama = %v", keys)
	}

	if !bytes.Equal(read(t, after, org, owner, old.ID), pdfBytes) {
		t.Error("berkas lama tidak terbaca setelah penyimpanan diganti")
	}
	if !bytes.Equal(read(t, after, org, owner, fresh.ID), newData) {
		t.Error("berkas baru tidak terbaca")
	}
	if list, err := after.List(ctx, org, owner); err != nil || len(list) != 2 || list[0].ID != old.ID || list[1].ID != fresh.ID {
		t.Errorf("List = %+v, %v", list, err)
	}

	if n, err := after.DeleteOwner(ctx, org, owner); err != nil || n != 2 {
		t.Fatalf("DeleteOwner = %d, %v; ingin 2", n, err)
	}
	// Yang tersisa hanya berkas media.
	if db, store := blobKeys(t, pool), mem.keys(); len(db) != 0 || !slices.Equal(store, []string{logoKey}) {
		t.Errorf("sisa isi: database %v, penyimpanan baru %v", db, store)
	}
}

// Galat penyimpanan selain "tidak ada" tidak boleh ditutupi dengan mencari
// ke database, dan tidak boleh tampil sebagai "tidak ditemukan": berkasnya
// akan tampak hilang, padahal penyimpanannya yang bermasalah.
func TestStoreFailureIsNotMissing(t *testing.T) {
	s := newService(t, attachments.Options{Store: failingOpen{newMemStore()}})
	ctx := context.Background()
	org := uuid.New()

	f, err := s.Save(ctx, org, workOrder(), "kontrak.pdf", uuid.Nil, bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Open(ctx, org, f.Owner, f.ID)
	if !errors.Is(err, errStoreDown) {
		t.Errorf("galat = %v, ingin galat penyimpanan", err)
	}
	if _, ok := errors.AsType[*appkit.Error](err); ok {
		t.Error("galat penyimpanan tidak boleh menjadi pesan untuk pengguna")
	}
}

// Serve selalu menyajikan berkas sebagai unduhan yang tidak disimpan, dan
// nama berkas — yang berasal dari pengguna — tidak dapat menyisipkan apa pun
// ke header.
func TestServe(t *testing.T) {
	serve := func(method string, f attachments.File, content []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		attachments.Serve(rec, httptest.NewRequest(method, "/v1/work-orders/1/attachments/"+f.ID.String(), nil), f, bytes.NewReader(content))
		return rec
	}

	for name, filename := range map[string]string{
		"biasa":          "kontrak.pdf",
		"spasi":          "kontrak kerja.pdf",
		"kutip":          `kontrak "final".pdf`,
		"garis miring":   `kontrak\final.pdf`,
		"sisipan":        `a.pdf"; filename="jahat.html`,
		"bukan ascii":    "surat perintah – naïve 日本.pdf",
		"kutip dan 日本":   `"日本"; filename=jahat.html`,
		"baris baru":     "a.pdf\r\nSet-Cookie: sesi=curian",
		"nama cadangan":  "berkas",
		"persen dan dll": "100% selesai; revisi=2.pdf",
	} {
		t.Run(name, func(t *testing.T) {
			f := attachments.File{ID: uuid.New(), Filename: filename, ContentType: "application/pdf", Size: int64(len(pdfBytes))}
			rec := serve(http.MethodGet, f, pdfBytes)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pdfBytes) {
				t.Fatalf("jawaban = %d dengan %d byte", rec.Code, rec.Body.Len())
			}
			for header, want := range map[string]string{
				"Content-Type":            "application/pdf",
				"Content-Length":          strconv.Itoa(len(pdfBytes)),
				"Cache-Control":           "private, no-store",
				"X-Content-Type-Options":  "nosniff",
				"Content-Security-Policy": "default-src 'none'; sandbox",
			} {
				if got := rec.Header().Get(header); got != want {
					t.Errorf("%s = %q, ingin %q", header, got, want)
				}
			}
			if len(rec.Header()) != 6 {
				t.Errorf("header = %v, ingin tepat enam", rec.Header())
			}

			raw := rec.Header().Get("Content-Disposition")
			// Header hanya berisi ASCII yang tercetak: tanpa baris baru, tanpa
			// huruf mentah di luar ASCII.
			for _, b := range []byte(raw) {
				if b < 0x20 || b > 0x7e {
					t.Fatalf("Content-Disposition memuat byte %#x: %q", b, raw)
				}
			}
			// Dibaca kembali, hasilnya tepat satu parameter: nama aslinya.
			kind, params, err := mime.ParseMediaType(raw)
			if err != nil {
				t.Fatalf("Content-Disposition %q tidak terbaca: %v", raw, err)
			}
			if kind != "attachment" {
				t.Errorf("Content-Disposition = %q, ingin attachment", raw)
			}
			if len(params) != 1 || params["filename"] != filename {
				t.Errorf("parameter = %v dari %q, ingin hanya filename %q", params, raw, filename)
			}
		})
	}

	t.Run("head tanpa isi", func(t *testing.T) {
		f := attachments.File{ID: uuid.New(), Filename: "kontrak.pdf", ContentType: "application/pdf", Size: int64(len(pdfBytes))}
		rec := serve(http.MethodHead, f, pdfBytes)
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Errorf("HEAD = %d dengan %d byte, ingin 200 tanpa isi", rec.Code, rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(pdfBytes)) {
			t.Errorf("Content-Length = %q", got)
		}
	})

	// Dari Save sampai Serve: yang tersaji adalah jenis hasil pengenalan isi,
	// bukan jenis menurut nama berkasnya.
	t.Run("berkas tersimpan", func(t *testing.T) {
		s := newService(t, attachments.Options{})
		ctx := context.Background()
		org := uuid.New()
		saved, err := s.Save(ctx, org, workOrder(), `dir/laporan "akhir" – 日本.png`, uuid.Nil, bytes.NewReader(pdfBytes))
		if err != nil {
			t.Fatal(err)
		}
		f, rc, err := s.Open(ctx, org, saved.Owner, saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		rec := httptest.NewRecorder()
		attachments.Serve(rec, httptest.NewRequest(http.MethodGet, "/", nil), f, rc)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pdfBytes) {
			t.Fatalf("jawaban = %d dengan %d byte", rec.Code, rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
			t.Errorf("Content-Type = %q", got)
		}
		_, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
		if err != nil || params["filename"] != `laporan "akhir" – 日本.png` {
			t.Errorf("filename = %q, %v", params["filename"], err)
		}
	})
}

// Identitas layanan S3 di compose.dev.yaml dan CI. Hanya berlaku di sana.
const (
	testAccessKey = "appkit"
	testSecretKey = "appkit-secret"
)

// s3Store mengembalikan s3store di layanan S3 test, dengan bucket baru yang
// dihapus saat test selesai — sama dengan serverOptions di test s3store. Test
// di-skip bila TEST_S3_ENDPOINT kosong; `make test` dan CI mengisinya.
func s3Store(t *testing.T) *s3store.Store {
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
	store, err := s3store.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Service di atas S3 sungguhan: isi berkas baru masuk ke bucket di bawah
// "private/", bukan ke database; lampiran yang isinya sudah di database
// sebelum berpindah tetap terbaca dan tetap dapat dihapus; dan penyaringan
// organization tidak bergantung pada penyimpanannya.
func TestServiceOnS3(t *testing.T) {
	store := s3Store(t)
	pool := testdb.New(t)
	ctx := context.Background()
	org, user := uuid.New(), uuid.New()
	owner := workOrder()

	// Sebelum berpindah: isi di database.
	before := newServiceOn(t, pool, attachments.Options{})
	old, err := before.Save(ctx, org, owner, "lama.pdf", user, bytes.NewReader(pdfBytes))
	if err != nil {
		t.Fatalf("Save ke database: %v", err)
	}

	after := newServiceOn(t, pool, attachments.Options{Store: store})
	newData := pngBytes(t)
	fresh, err := after.Save(ctx, org, owner, "baru.png", user, bytes.NewReader(newData))
	if err != nil {
		t.Fatalf("Save ke S3: %v", err)
	}
	if keys := blobKeys(t, pool); len(keys) != 1 {
		t.Errorf("isi di database = %v setelah Save ke S3, ingin tetap 1", keys)
	}
	freshKey := "private/" + org.String() + "/" + fresh.ID.String()
	rc, err := store.Open(ctx, freshKey)
	if err != nil {
		t.Fatalf("isi berkas baru tidak ada di bucket: %v", err)
	}
	inBucket, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(inBucket, newData) {
		t.Error("isi di bucket berbeda dari yang disimpan")
	}

	for name, tc := range map[string]struct {
		id   uuid.UUID
		data []byte
	}{"lama": {old.ID, pdfBytes}, "baru": {fresh.ID, newData}} {
		if !bytes.Equal(read(t, after, org, owner, tc.id), tc.data) {
			t.Errorf("isi berkas %s berbeda dari yang disimpan", name)
		}
	}
	if list, err := after.List(ctx, org, owner); err != nil || len(list) != 2 || !same(list[0], old) || !same(list[1], fresh) {
		t.Errorf("List = %+v, %v", list, err)
	}
	usage, err := after.Usage(ctx, org)
	if err != nil || usage.Files != 2 || usage.Bytes != int64(len(pdfBytes)+len(newData)) {
		t.Errorf("Usage = %+v, %v", usage, err)
	}

	// Organization lain tidak dapat membuka maupun menghapusnya.
	other := uuid.New()
	_, _, err = after.Open(ctx, other, owner, fresh.ID)
	wantNotFound(t, "Open lintas organization", err)
	if err := after.Delete(ctx, other, owner, fresh.ID); err != nil {
		t.Fatalf("Delete lintas organization: %v", err)
	}
	if !bytes.Equal(read(t, after, org, owner, fresh.ID), newData) {
		t.Error("berkas terhapus oleh organization lain")
	}

	if n, err := after.DeleteOwner(ctx, org, owner); err != nil || n != 2 {
		t.Fatalf("DeleteOwner = %d, %v; ingin 2", n, err)
	}
	if keys := blobKeys(t, pool); len(keys) != 0 {
		t.Errorf("isi di database = %v setelah dihapus, ingin kosong", keys)
	}
	if _, err := store.Open(ctx, freshKey); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("isi di bucket setelah dihapus = %v, ingin fs.ErrNotExist", err)
	}
}
