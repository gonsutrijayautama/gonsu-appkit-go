package website_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/businessprofile"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
	"github.com/gonsutrijayautama/gonsu-appkit-go/website"
)

type fixture struct {
	sites    *website.Service
	profiles *businessprofile.Service
	media    *media.Service
	trail    *audit.Service
	// org adalah organization halaman publik (tanpa sesi).
	org uuid.UUID
}

func setup(t *testing.T, ttl time.Duration) *fixture {
	t.Helper()
	pool := testdb.New(t)
	f := &fixture{org: uuid.New()}
	var err error
	if f.media, err = media.New(pool, testdb.Hooks(), media.Options{MaxBytes: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	f.trail = testdb.Trail(t, pool)
	if f.profiles, err = businessprofile.New(pool, f.media, f.trail, testdb.Hooks()); err != nil {
		t.Fatal(err)
	}
	f.sites, err = website.New(pool, f.profiles, f.media, f.trail, testdb.Hooks(), website.Options{
		PublicOrganization: func(*http.Request) (uuid.UUID, error) { return f.org, nil },
		CacheTTL:           ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func admin(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{
		Organization: org,
		User:         uuid.New(),
		Permissions:  []appkit.Permission{website.Manage, businessprofile.Manage},
	})
}

func staff(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{Organization: org})
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	e, ok := errors.AsType[*appkit.Error](err)
	if !ok || e.Kind != appkit.KindValidation {
		t.Fatalf("galat = %v, ingin galat validasi", err)
	}
	out := map[string]string{}
	for _, f := range e.Fields {
		out[f.Field] = f.Message
	}
	return out
}

// siteInput adalah identitas lengkap sebuah halaman publik.
func siteInput() website.Input {
	return website.Input{
		Mode:    website.ModeSite,
		Tagline: "Pakaian rapi untuk setiap hari",
		Summary: "Toko pakaian keluarga di Bandung sejak 2010.",
		Contact: website.Contact{Hours: "Senin–Sabtu 09.00–17.00", MapURL: "https://maps.app.goo.gl/abc"},
		Channels: website.Channels{
			WhatsApp:  "0812-3456-7890",
			Instagram: "https://www.instagram.com/tokobaju",
		},
	}
}

func TestGetBeforeAnythingIsSaved(t *testing.T) {
	f := setup(t, 0)
	s, err := f.sites.Get(staff(uuid.New()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Bawaannya hanya pintu masuk: tidak ada web publik tanpa diminta.
	if s.Mode != website.ModeSignIn || s.Version != 0 || s.UpdatedAt != nil {
		t.Errorf("pengaturan yang belum disimpan = %+v", s)
	}
	raw, _ := json.Marshal(s)
	for _, field := range []string{`"mode":"signin"`, `"image":null`, `"version":0`, `"updated_at":null`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("JSON tidak memuat %s: %s", field, raw)
		}
	}
	// Yang disimpan di sini hanya identitas: isi halaman bukan urusan modul ini.
	for _, field := range []string{`"about"`, `"services"`, `"icons"`} {
		if strings.Contains(string(raw), field) {
			t.Errorf("JSON masih memuat %s: %s", field, raw)
		}
	}
}

func TestUpdate(t *testing.T) {
	f := setup(t, 0)
	org := uuid.New()

	s, err := f.sites.Update(admin(org), siteInput())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if s.Mode != website.ModeSite || s.Version != 1 || s.UpdatedAt == nil || s.Contact.Hours == "" {
		t.Errorf("pengaturan tersimpan = %+v", s)
	}
	// Nomor WhatsApp disimpan sebagai digit dengan kode negara.
	if s.Channels.WhatsApp != "6281234567890" {
		t.Errorf("whatsapp = %q", s.Channels.WhatsApp)
	}

	// Menyimpan mengganti SELURUH isian.
	s, err = f.sites.Update(admin(org), website.Input{Mode: website.ModeSite, Tagline: "Baru", Version: s.Version})
	if err != nil || s.Version != 2 {
		t.Fatalf("Update kedua = %+v, %v", s, err)
	}
	if s.Summary != "" || s.Contact.Hours != "" || s.Channels.Instagram != "" {
		t.Errorf("isian lama tertinggal: %+v", s)
	}
	if got, err := f.sites.Get(staff(org)); err != nil || got.Tagline != "Baru" {
		t.Errorf("Get oleh staf = %+v, %v", got, err)
	}
}

func TestUpdateValidation(t *testing.T) {
	f := setup(t, 0)
	ctx := admin(uuid.New())
	long := strings.Repeat("a", 2001)
	with := func(change func(*website.Input)) website.Input {
		in := siteInput()
		change(&in)
		return in
	}

	for name, tc := range map[string]struct {
		in    website.Input
		field string
	}{
		"jam kerja panjang":       {with(func(in *website.Input) { in.Contact.Hours = long }), "contact.hours"},
		"mode tak dikenal":        {with(func(in *website.Input) { in.Mode = "toko" }), "mode"},
		"tagline panjang":         {with(func(in *website.Input) { in.Tagline = long }), "tagline"},
		"ringkasan panjang":       {with(func(in *website.Input) { in.Summary = long }), "summary"},
		"peta bukan https":        {with(func(in *website.Input) { in.Contact.MapURL = "http://maps.example/x" }), "contact.map_url"},
		"peta skema javascript":   {with(func(in *website.Input) { in.Contact.MapURL = "javascript:alert(1)" }), "contact.map_url"},
		"whatsapp berisi huruf":   {with(func(in *website.Input) { in.Channels.WhatsApp = "nol delapan" }), "channels.whatsapp"},
		"whatsapp terlalu pendek": {with(func(in *website.Input) { in.Channels.WhatsApp = "0812" }), "channels.whatsapp"},
		"instagram di situs lain": {with(func(in *website.Input) { in.Channels.Instagram = "https://evil.example/tokobaju" }), "channels.instagram"},
		"instagram host mirip":    {with(func(in *website.Input) { in.Channels.Instagram = "https://notinstagram.com/x" }), "channels.instagram"},
		"tiktok hanya nama akun":  {with(func(in *website.Input) { in.Channels.TikTok = "@tokobaju" }), "channels.tiktok"},
		"judul SEO panjang":       {with(func(in *website.Input) { in.SEO.Title = strings.Repeat("a", 71) }), "seo.title"},
		"deskripsi SEO panjang":   {with(func(in *website.Input) { in.SEO.Description = strings.Repeat("a", 161) }), "seo.description"},
		"version negatif":         {with(func(in *website.Input) { in.Version = -1 }), "version"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.sites.Update(ctx, tc.in)
			if fields := fieldErrors(t, err); fields[tc.field] == "" {
				t.Errorf("tidak ada galat untuk %s: %v", tc.field, fields)
			}
		})
	}
	if s, _ := f.sites.Get(ctx); s.UpdatedAt != nil {
		t.Errorf("isian yang tidak sah tersimpan: %+v", s)
	}
}

// Dua pengelola membuka formulir yang sama: yang menyimpan belakangan
// ditolak, bukan menimpa diam-diam.
func TestUpdateRejectsStaleVersion(t *testing.T) {
	f := setup(t, 0)
	ctx := admin(uuid.New())
	conflict := func(err error) bool {
		e, ok := errors.AsType[*appkit.Error](err)
		return ok && e.Kind == appkit.KindConflict
	}

	first, err := f.sites.Update(ctx, siteInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sites.Update(ctx, siteInput()); !conflict(err) {
		t.Errorf("simpan dengan version 0 sesudah ada pengaturan = %v, ingin konflik", err)
	}
	next := siteInput()
	next.Version = first.Version
	if s, err := f.sites.Update(ctx, next); err != nil || s.Version != 2 {
		t.Fatalf("simpan dengan version terbaru = %+v, %v", s, err)
	}
	if _, err := f.sites.Update(ctx, next); !conflict(err) {
		t.Errorf("simpan dengan version lama = %v, ingin konflik", err)
	}
}

func TestOnlyManageMayChange(t *testing.T) {
	f := setup(t, 0)
	org := uuid.New()
	if _, err := f.sites.Update(staff(org), siteInput()); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("Update tanpa izin = %v", err)
	}
	if _, err := f.sites.SetImage(staff(org), website.SlotSEO, bytes.NewReader(pngBytes(t))); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("SetImage tanpa izin = %v", err)
	}
	if _, err := f.sites.RemoveImage(staff(org), website.SlotSEO); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("RemoveImage tanpa izin = %v", err)
	}
	if _, err := f.sites.Get(context.Background()); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Get tanpa sesi = %v", err)
	}
	if s, _ := f.sites.Get(staff(org)); s.UpdatedAt != nil {
		t.Error("percobaan tanpa izin meninggalkan baris pengaturan")
	}
}

func TestTenantIsolation(t *testing.T) {
	f := setup(t, 0)
	a, b := uuid.New(), uuid.New()
	if _, err := f.sites.Update(admin(a), siteInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sites.SetImage(admin(a), website.SlotSEO, bytes.NewReader(pngBytes(t))); err != nil {
		t.Fatal(err)
	}
	if s, err := f.sites.Get(admin(b)); err != nil || s.Tagline != "" || s.SEO.Image != nil || s.Mode != website.ModeSignIn {
		t.Errorf("organization B melihat pengaturan A: %+v, %v", s, err)
	}
	if _, err := f.sites.RemoveImage(admin(b), website.SlotSEO); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.sites.Get(admin(a)); s.Tagline == "" || s.SEO.Image == nil {
		t.Errorf("pengaturan A berubah oleh organization B: %+v", s)
	}
	// Tampilan publik pun terpisah.
	if site, err := f.sites.Public(context.Background(), b); err != nil || site.Mode != website.ModeSignIn || site.Tagline != "" {
		t.Errorf("tampilan publik B = %+v, %v", site, err)
	}
}

func TestImages(t *testing.T) {
	f := setup(t, 0)
	ctx := admin(uuid.New())

	// Gambar boleh diunggah sebelum pengaturan disimpan, dan tidak menaikkan
	// version: formulir yang sedang terbuka tetap dapat disimpan.
	s, err := f.sites.SetImage(ctx, website.SlotSEO, bytes.NewReader(pngBytes(t)))
	if err != nil || s.SEO.Image == nil || s.Version != 0 {
		t.Fatalf("SetImage = %+v, %v", s, err)
	}
	first := s.SEO.Image.ID
	if s, err = f.sites.Update(ctx, siteInput()); err != nil || s.SEO.Image == nil || s.SEO.Image.ID != first {
		t.Fatalf("gambar setelah Update = %+v, %v", s.SEO.Image, err)
	}

	// Mengganti: id baru, berkas lama terhapus.
	s, err = f.sites.SetImage(ctx, website.SlotSEO, bytes.NewReader(pngBytes(t)))
	if err != nil || s.SEO.Image.ID == first {
		t.Fatalf("gambar pengganti = %+v, %v", s.SEO.Image, err)
	}
	if _, _, err := f.media.Open(context.Background(), first); err == nil {
		t.Error("berkas gambar lama masih ada")
	}

	if _, err := f.sites.SetImage(ctx, website.SlotSEO, strings.NewReader("<svg/>")); err == nil {
		t.Error("SVG diterima sebagai gambar")
	}
	// Yang ada hanya gambar pratinjau tautan: gambar bagian halaman bukan
	// identitas, dan nama kolom tidak pernah dapat dipakai sebagai slot.
	for _, slot := range []website.Slot{"logo", "", "about", "seo_media_id"} {
		_, err := f.sites.SetImage(ctx, slot, bytes.NewReader(pngBytes(t)))
		if e, ok := errors.AsType[*appkit.Error](err); !ok || e.Kind != appkit.KindNotFound {
			t.Errorf("slot %q = %v, ingin tidak ditemukan", slot, err)
		}
	}

	second := s.SEO.Image.ID
	if s, err = f.sites.RemoveImage(ctx, website.SlotSEO); err != nil || s.SEO.Image != nil {
		t.Fatalf("RemoveImage = %+v, %v", s, err)
	}
	if _, _, err := f.media.Open(context.Background(), second); err == nil {
		t.Error("berkas gambar masih ada setelah dihapus")
	}
	if _, err := f.sites.RemoveImage(ctx, website.SlotSEO); err != nil {
		t.Errorf("RemoveImage kedua: %v", err)
	}
}

// profile menyimpan profil bisnis organization halaman publik.
func (f *fixture) profile(t *testing.T) {
	t.Helper()
	_, err := f.profiles.Update(admin(f.org), businessprofile.Input{
		DisplayName: "Toko Baju Sejahtera", Industry: "Ritel pakaian",
		Email: "halo@tokobaju.example", Phone: "(022) 123-4567",
		BusinessType: businessprofile.TypeCompany, LegalName: "PT Baju Sejahtera Makmur", TaxID: "0012345678901000",
		Address: "Jl. Pasteur No. 10", RegionCode: "32.73.07.1001",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPublic(t *testing.T) {
	f := setup(t, 0)
	ctx := context.Background()
	f.profile(t)
	if _, err := f.profiles.SetLogo(admin(f.org), bytes.NewReader(pngBytes(t))); err != nil {
		t.Fatal(err)
	}

	// Bawaan (hanya pintu masuk): nama, logo, dan judul saja — organization
	// ini belum memilih punya web publik.
	site, err := f.sites.Public(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	if site.Mode != website.ModeSignIn || site.Name != "Toko Baju Sejahtera" || site.LogoURL == "" || site.SEO.Title != "Toko Baju Sejahtera" {
		t.Errorf("tampilan pintu masuk = %+v", site)
	}
	if site.Industry != "" || site.Contact.Email != "" || site.Contact.Address != "" || site.Contact.City != "" {
		t.Errorf("mode pintu masuk membuka kontak: %+v", site)
	}

	if _, err := f.sites.Update(admin(f.org), siteInput()); err != nil {
		t.Fatal(err)
	}
	site, err = f.sites.Public(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	if site.Mode != website.ModeSite || site.Industry != "Ritel pakaian" || site.Tagline != "Pakaian rapi untuk setiap hari" {
		t.Errorf("tampilan web = %+v", site)
	}
	wantContact := website.PublicContact{
		Address: "Jl. Pasteur No. 10, Desa Pasteur, Kecamatan Sukajadi, Kota Bandung, Jawa Barat 40161",
		City:    "Kota Bandung", Email: "halo@tokobaju.example", Phone: "(022) 123-4567",
		Hours: "Senin–Sabtu 09.00–17.00", MapURL: "https://maps.app.goo.gl/abc",
	}
	if site.Contact != wantContact {
		t.Errorf("kontak = %+v", site.Contact)
	}
	// WhatsApp menjadi tautan siap pakai.
	if site.Channels.WhatsApp != "https://wa.me/6281234567890" || site.Channels.Instagram != "https://www.instagram.com/tokobaju" {
		t.Errorf("kanal = %+v", site.Channels)
	}
	// Judul dan deskripsi diturunkan dari isi; gambarnya logo.
	if site.SEO.Title != "Toko Baju Sejahtera — Pakaian rapi untuk setiap hari" ||
		site.SEO.Description != "Toko pakaian keluarga di Bandung sejak 2010." || site.SEO.ImageURL != site.LogoURL {
		t.Errorf("SEO turunan = %+v", site.SEO)
	}

	// NPWP dan nama legal tidak pernah keluar ke publik.
	raw, _ := json.Marshal(site)
	for _, secret := range []string{"0012345678901000", "PT Baju Sejahtera Makmur", "tax_id", "legal_name"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("tampilan publik memuat %q: %s", secret, raw)
		}
	}

	// Alamat disembunyikan: kota tetap tampil. SEO yang diisi mengalahkan turunan.
	in := siteInput()
	in.Version = 1
	in.Contact.HideAddress = true
	in.SEO.Title, in.SEO.Description = "Toko Baju Bandung", "Belanja pakaian keluarga."
	if _, err := f.sites.Update(admin(f.org), in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sites.SetImage(admin(f.org), website.SlotSEO, bytes.NewReader(pngBytes(t))); err != nil {
		t.Fatal(err)
	}
	site, _ = f.sites.Public(ctx, f.org)
	if site.Contact.Address != "" || site.Contact.City != "Kota Bandung" {
		t.Errorf("alamat disembunyikan = %+v", site.Contact)
	}
	if site.SEO.Title != "Toko Baju Bandung" || site.SEO.Description != "Belanja pakaian keluarga." ||
		site.SEO.ImageURL == "" || site.SEO.ImageURL == site.LogoURL {
		t.Errorf("SEO yang diisi = %+v", site.SEO)
	}
}

// Tampilan publik disajikan dari memori selama masih segar, dan perubahan
// pengaturan di proses ini langsung membuangnya.
func TestPublicIsCached(t *testing.T) {
	f := setup(t, time.Hour)
	ctx := context.Background()
	f.profile(t)
	if _, err := f.sites.Update(admin(f.org), siteInput()); err != nil {
		t.Fatal(err)
	}
	if site, _ := f.sites.Public(ctx, f.org); site.Name != "Toko Baju Sejahtera" {
		t.Fatalf("tampilan = %+v", site)
	}

	// Profil berubah lewat service lain: belum terlihat sampai umurnya habis.
	if _, err := f.profiles.Update(admin(f.org), businessprofile.Input{DisplayName: "Nama Baru", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if site, _ := f.sites.Public(ctx, f.org); site.Name != "Toko Baju Sejahtera" {
		t.Errorf("tampilan tidak dari memori: %+v", site.Name)
	}
	// Pengaturan website berubah: memori dibuang, nama baru ikut terbaca.
	in := siteInput()
	in.Version = 1
	if _, err := f.sites.Update(admin(f.org), in); err != nil {
		t.Fatal(err)
	}
	if site, _ := f.sites.Public(ctx, f.org); site.Name != "Nama Baru" {
		t.Errorf("tampilan sesudah pengaturan berubah = %q", site.Name)
	}
}

const page = `<!DOCTYPE html><html lang="id"><head><meta charSet="utf-8"/><title>Produk Contoh</title>` +
	`<meta name="description" content="Bawaan build."/><link rel="icon" href="/favicon.ico"/></head><body><main>isi</main></body></html>`

func TestInjectHTML(t *testing.T) {
	site := website.Public{
		Mode: website.ModeSite, Name: `Toko "Baju" <b>`, Tagline: "A & B",
		SEO: website.PublicSEO{
			Title: `Toko "Baju" <b> — A & B`, Description: `Harga $1 </script><script>alert(1)</script>`,
			ImageURL: "/media/0b2f3a6e-0000-4000-8000-000000000001",
		},
	}
	out := string(website.InjectHTML([]byte(page), site, "https://toko.example"))

	for _, want := range []string{
		`<title>Toko &#34;Baju&#34; &lt;b&gt; — A &amp; B</title>`,
		`<meta name="description" content="Harga $1 &lt;/script&gt;&lt;script&gt;alert(1)&lt;/script&gt;"/>`,
		`<meta property="og:title" content="Toko &#34;Baju&#34; &lt;b&gt; — A &amp; B"/>`,
		`<meta property="og:url" content="https://toko.example/"/>`,
		`<meta property="og:image" content="https://toko.example/media/0b2f3a6e-0000-4000-8000-000000000001"/>`,
		`<script id="gonsu-site" type="application/json">`,
		`<main>isi</main>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("halaman tidak memuat %s\n%s", want, out)
		}
	}
	// Judul dan deskripsi bawaan build diganti, bukan digandakan.
	if strings.Contains(out, "Produk Contoh") || strings.Contains(out, "Bawaan build.") ||
		strings.Count(out, "<title>") != 1 || strings.Count(out, `name="description"`) != 1 {
		t.Errorf("judul atau deskripsi bawaan tersisa:\n%s", out)
	}
	// Isi apa pun tidak dapat menutup elemen script dan menyisipkan skrip.
	if strings.Count(out, "</script>") != 1 || strings.Contains(out, "<script>alert") {
		t.Errorf("isian menembus elemen script:\n%s", out)
	}
	// Data yang disisipkan terbaca kembali persis.
	start := strings.Index(out, `type="application/json">`) + len(`type="application/json">`)
	var got website.Public
	if err := json.Unmarshal([]byte(out[start:start+strings.Index(out[start:], "</script>")]), &got); err != nil {
		t.Fatalf("data tersisip bukan JSON: %v", err)
	}
	if got.Name != site.Name || got.SEO.Description != site.SEO.Description {
		t.Errorf("data tersisip = %+v", got)
	}

	// Tanpa pengganti, judul dan deskripsi build dipertahankan.
	plain := string(website.InjectHTML([]byte(page), website.Public{Mode: website.ModeSignIn}, ""))
	if !strings.Contains(plain, "<title>Produk Contoh</title>") || !strings.Contains(plain, "Bawaan build.") || strings.Contains(plain, "og:image") {
		t.Errorf("halaman tanpa data SEO:\n%s", plain)
	}
	// Halaman tanpa </head> dikembalikan apa adanya.
	if got := website.InjectHTML([]byte("<p>halo</p>"), site, ""); string(got) != "<p>halo</p>" {
		t.Errorf("halaman tanpa head = %s", got)
	}
}

func TestRenderHome(t *testing.T) {
	f := setup(t, 0)
	f.profile(t)
	if _, err := f.sites.Update(admin(f.org), siteInput()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://toko.example/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	out := string(f.sites.RenderHome(req, []byte(page)))
	for _, want := range []string{
		"<title>Toko Baju Sejahtera — Pakaian rapi untuk setiap hari</title>",
		`<meta property="og:url" content="https://toko.example/"/>`,
		`"mode":"site"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("halaman depan tidak memuat %s\n%s", want, out)
		}
	}
}

// Halaman depan adalah probe platform: organization yang tidak diketahui
// atau database yang mati tidak boleh membuatnya gagal.
func TestRenderHomeNeverFails(t *testing.T) {
	pool := testdb.New(t)
	m, _ := media.New(pool, testdb.Hooks(), media.Options{})
	trail := testdb.Trail(t, pool)
	profiles, _ := businessprofile.New(pool, m, trail, testdb.Hooks())
	unknown := errors.New("organization belum diketahui")
	orgErr := error(unknown)
	sites, err := website.New(pool, profiles, m, trail, testdb.Hooks(), website.Options{
		PublicOrganization: func(*http.Request) (uuid.UUID, error) { return uuid.New(), orgErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := sites.RenderHome(req, []byte(page)); string(got) != page {
		t.Errorf("organization tidak diketahui: halaman berubah")
	}

	orgErr = nil
	pool.Close() // database "mati"
	if got := sites.RenderHome(req, []byte(page)); string(got) != page {
		t.Errorf("database mati: halaman berubah")
	}
}

func TestRoutes(t *testing.T) {
	f := setup(t, 0)
	f.profile(t)
	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", f.sites.Routes()...)
	appkit.Register(mux, "", f.sites.PublicRoutes()...)

	do := func(ctx context.Context, method, path string, body io.Reader) (int, website.Settings, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, body).WithContext(ctx))
		var s website.Settings
		_ = json.Unmarshal(rec.Body.Bytes(), &s)
		return rec.Code, s, rec.Body.String()
	}
	org := f.org

	if code, s, _ := do(staff(org), http.MethodGet, "/v1/website", nil); code != http.StatusOK || s.Mode != website.ModeSignIn {
		t.Errorf("GET sebelum disimpan = %d, %+v", code, s)
	}
	body := `{"mode":"site","tagline":"Rapi setiap hari","contact":{"hours":"Senin–Jumat","map_url":"","hide_address":true},"version":0}`
	if code, _, _ := do(staff(org), http.MethodPut, "/v1/website", strings.NewReader(body)); code != http.StatusForbidden {
		t.Errorf("PUT tanpa izin = %d, ingin 403", code)
	}
	code, s, _ := do(admin(org), http.MethodPut, "/v1/website", strings.NewReader(body))
	if code != http.StatusOK || s.Version != 1 || !s.Contact.HideAddress {
		t.Fatalf("PUT = %d, %+v", code, s)
	}
	// Version lama: 409, bukan menimpa.
	if code, _, _ := do(admin(org), http.MethodPut, "/v1/website", strings.NewReader(body)); code != http.StatusConflict {
		t.Errorf("PUT dengan version lama = %d, ingin 409", code)
	}
	// Gambar tidak diatur lewat simpan: field-nya ditolak, bukan diabaikan.
	code, _, raw := do(admin(org), http.MethodPut, "/v1/website", strings.NewReader(`{"mode":"site","seo":{"title":"x","image":{"id":"x"}},"version":1}`))
	if code != http.StatusBadRequest || !strings.Contains(raw, "image") {
		t.Errorf("gambar lewat simpan = %d, %s", code, raw)
	}
	// Isi halaman sudah bukan bagian pengaturan ini: field lamanya ditolak.
	for _, old := range []string{`"about":{"text":"x"}`, `"services":[]`} {
		code, _, raw := do(admin(org), http.MethodPut, "/v1/website", strings.NewReader(`{"mode":"site",`+old+`,"version":1}`))
		if code != http.StatusBadRequest {
			t.Errorf("field lama %s = %d, %s", old, code, raw)
		}
	}

	if code, _, _ := do(staff(org), http.MethodPut, "/v1/website/images/seo", bytes.NewReader(pngBytes(t))); code != http.StatusForbidden {
		t.Errorf("unggah gambar tanpa izin = %d, ingin 403", code)
	}
	code, s, _ = do(admin(org), http.MethodPut, "/v1/website/images/seo", bytes.NewReader(pngBytes(t)))
	if code != http.StatusOK || s.SEO.Image == nil {
		t.Fatalf("unggah gambar = %d, %+v", code, s.SEO)
	}
	for _, slot := range []string{"logo", "about"} {
		if code, _, _ := do(admin(org), http.MethodPut, "/v1/website/images/"+slot, bytes.NewReader(pngBytes(t))); code != http.StatusNotFound {
			t.Errorf("slot %s = %d, ingin 404", slot, code)
		}
	}
	if code, s, _ := do(admin(org), http.MethodDelete, "/v1/website/images/seo", nil); code != http.StatusOK || s.SEO.Image != nil {
		t.Errorf("hapus gambar = %d, %+v", code, s.SEO)
	}

	// Tampilan publik: tanpa sesi sama sekali.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/site.json", nil))
	var site website.Public
	if err := json.Unmarshal(rec.Body.Bytes(), &site); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /site.json = %d, %v", rec.Code, err)
	}
	if site.Mode != website.ModeSite || site.Name != "Toko Baju Sejahtera" || site.Tagline != "Rapi setiap hari" {
		t.Errorf("tampilan publik = %+v", site)
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Error("tampilan publik tanpa Cache-Control")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	pool := testdb.New(t)
	m, _ := media.New(pool, testdb.Hooks(), media.Options{})
	trail := testdb.Trail(t, pool)
	profiles, _ := businessprofile.New(pool, m, trail, testdb.Hooks())
	opts := website.Options{PublicOrganization: func(*http.Request) (uuid.UUID, error) { return uuid.Nil, nil }}

	if _, err := website.New(nil, profiles, m, trail, testdb.Hooks(), opts); err == nil {
		t.Error("New tanpa pool lolos")
	}
	if _, err := website.New(pool, nil, m, trail, testdb.Hooks(), opts); err == nil {
		t.Error("New tanpa profil bisnis lolos")
	}
	if _, err := website.New(pool, profiles, nil, trail, testdb.Hooks(), opts); err == nil {
		t.Error("New tanpa media lolos")
	}
	if _, err := website.New(pool, profiles, m, nil, testdb.Hooks(), opts); err == nil {
		t.Error("New tanpa jejak audit lolos")
	}
	if _, err := website.New(pool, profiles, m, trail, appkit.Hooks{}, opts); err == nil {
		t.Error("New tanpa pengait lolos")
	}
	if _, err := website.New(pool, profiles, m, trail, testdb.Hooks(), website.Options{}); err == nil {
		t.Error("New tanpa PublicOrganization lolos")
	}
}

// Kuota penyimpanan berlaku untuk gambar website seperti untuk logo: gambar
// baru ditolak saat penuh, gambar pengganti di slot yang sama tetap masuk.
func TestImageQuota(t *testing.T) {
	pool := testdb.New(t)
	img := pngBytes(t)
	m, err := media.New(pool, testdb.Hooks(), media.Options{
		Quota: func(context.Context, uuid.UUID) (int64, error) { return int64(len(img)), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	trail := testdb.Trail(t, pool)
	profiles, err := businessprofile.New(pool, m, trail, testdb.Hooks())
	if err != nil {
		t.Fatal(err)
	}
	org := uuid.New()
	sites, err := website.New(pool, profiles, m, trail, testdb.Hooks(), website.Options{
		PublicOrganization: func(*http.Request) (uuid.UUID, error) { return org, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := admin(org)

	s, err := sites.SetImage(ctx, website.SlotSEO, bytes.NewReader(img))
	if err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	first := s.SEO.Image.ID

	// Berkas lain di organization yang sama (logo): tambahan, kuota tidak cukup.
	p, err := profiles.SetLogo(ctx, bytes.NewReader(img))
	if e, ok := errors.AsType[*appkit.Error](err); !ok || e.Kind != appkit.KindQuotaExceeded {
		t.Fatalf("berkas kedua saat kuota penuh = %v, ingin galat kuota penuh", err)
	}
	if p.Logo != nil {
		t.Errorf("logo terpasang oleh unggahan yang ditolak: %+v", p.Logo)
	}

	// Slot yang sama: pengganti, pemakaian tidak bertambah.
	s, err = sites.SetImage(ctx, website.SlotSEO, bytes.NewReader(img))
	if err != nil || s.SEO.Image == nil || s.SEO.Image.ID == first {
		t.Fatalf("mengganti gambar saat kuota penuh = %+v, %v", s.SEO.Image, err)
	}
	if usage, _ := m.Usage(ctx, org); usage.Files != 1 {
		t.Errorf("pemakaian = %+v, ingin 1 berkas", usage)
	}
}

// Setiap perubahan pengaturan website masuk jejak audit, di transaksi yang
// sama: yang dicatat adalah isian mana yang berubah, bukan isinya.
func TestChangesAreRecorded(t *testing.T) {
	f := setup(t, time.Minute)
	org := uuid.New()
	ctx := admin(org)

	in := siteInput()
	first, err := f.sites.Update(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Version, in.Tagline = first.Version, "Tagline baru"
	in.Contact.HideAddress = true
	second, err := f.sites.Update(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// Simpan tanpa perubahan, simpan yang ditolak, dan menghapus gambar yang
	// tidak ada tidak meninggalkan catatan.
	in.Version = second.Version
	if _, err := f.sites.Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Version = first.Version
	if _, err := f.sites.Update(ctx, in); err == nil {
		t.Fatal("simpan dengan version lama lolos")
	}
	if _, err := f.sites.RemoveImage(ctx, website.SlotSEO); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sites.SetImage(ctx, website.SlotSEO, bytes.NewReader(pngBytes(t))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sites.RemoveImage(ctx, website.SlotSEO); err != nil {
		t.Fatal(err)
	}

	events := testdb.Recorded(t, f.trail, org)
	if len(events) != 4 {
		t.Fatalf("tindakan tercatat = %v, ingin 4", testdb.Actions(t, f.trail, org))
	}
	raw, _ := json.Marshal([]any{events[0].Details, events[1].Details, events[2].Details, events[3].Details})
	want := `[{"fields":["mode","tagline","summary","contact","channels"]},` +
		`{"fields":["tagline","contact"]},{"slot":"seo"},{"slot":"seo"}]`
	if string(raw) != want {
		t.Errorf("rincian = %s\ningin     %s", raw, want)
	}
	if events[0].Action != website.ActionUpdated || events[0].Category != audit.CategorySettings || events[0].ActorID == nil ||
		events[2].Action != website.ActionImageChanged || events[3].Action != website.ActionImageRemoved {
		t.Errorf("catatan = %+v", events)
	}
	// Isinya tidak pernah masuk catatan.
	if all, _ := json.Marshal(events); strings.Contains(string(all), "Tagline baru") {
		t.Errorf("isi pengaturan masuk jejak audit: %s", all)
	}

	// Pemegang izin tanpa identitas pengguna tidak dapat mengubah.
	anonymous := testdb.With(context.Background(), testdb.Session{Organization: org, Permissions: []appkit.Permission{website.Manage}})
	in.Version, in.Tagline = second.Version+1, "Tanpa pelaku"
	if _, err := f.sites.Update(anonymous, in); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Update tanpa pengguna = %v", err)
	}
	if got, _ := f.sites.Get(ctx); got.Tagline != "Tagline baru" {
		t.Errorf("perubahan tanpa pelaku tersimpan: %+v", got.Tagline)
	}
}
