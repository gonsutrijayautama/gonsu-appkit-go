package businessprofile_test

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

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/businessprofile"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
)

type fixture struct {
	profiles *businessprofile.Service
	media    *media.Service
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	m, err := media.New(pool, testdb.Hooks(), media.Options{MaxBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	s, err := businessprofile.New(pool, m, testdb.Hooks())
	if err != nil {
		t.Fatal(err)
	}
	return fixture{profiles: s, media: m}
}

// admin dan staff: dua pengguna satu organization, hanya admin yang memegang
// izin Manage.
func admin(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{
		Organization: org, Permissions: []appkit.Permission{businessprofile.Manage},
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

func TestGetBeforeAnythingIsSaved(t *testing.T) {
	f := setup(t)
	p, err := f.profiles.Get(staff(uuid.New()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.DisplayName != "" || p.UpdatedAt != nil || p.Region != nil || p.Logo != nil {
		t.Errorf("profil yang belum disimpan = %+v", p)
	}
	// Bentuk JSON-nya tetap lengkap: klien tidak perlu menebak field yang hilang.
	raw, _ := json.Marshal(p)
	for _, field := range []string{`"display_name":""`, `"region":null`, `"logo":null`, `"updated_at":null`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("JSON tidak memuat %s: %s", field, raw)
		}
	}
}

func TestUpdate(t *testing.T) {
	f := setup(t)
	org := uuid.New()

	p, err := f.profiles.Update(admin(org), businessprofile.Input{
		DisplayName:  "  Toko Baju Sejahtera ",
		Industry:     "Ritel pakaian",
		Email:        "halo@tokobaju.example",
		Phone:        "(022) 123-4567",
		BusinessType: businessprofile.TypeCompany,
		LegalName:    "PT Baju Sejahtera Makmur",
		TaxID:        "01.234.567.8-901.000",
		Address:      "Jl. Pasteur No. 10\r\nRT 01/RW 02",
		RegionCode:   "32.73.07.1001",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if p.DisplayName != "Toko Baju Sejahtera" {
		t.Errorf("display_name = %q, ingin tanpa spasi tepi", p.DisplayName)
	}
	// NPWP 15 digit lama disimpan 16 digit tanpa pemisah.
	if p.TaxID != "0012345678901000" {
		t.Errorf("tax_id = %q", p.TaxID)
	}
	// Kode pos terisi dari desa yang dipilih.
	if p.Postcode != "40161" {
		t.Errorf("postcode = %q, ingin kode pos desa", p.Postcode)
	}
	if p.Region == nil || p.Region.Regency.Name != "Kota Bandung" || p.Region.Province.Name != "Jawa Barat" ||
		p.Region.District == nil || p.Region.Village == nil || p.Region.Village.Name != "Pasteur" {
		t.Errorf("region = %+v", p.Region)
	}
	want := "Jl. Pasteur No. 10, RT 01/RW 02, Desa Pasteur, Kecamatan Sukajadi, Kota Bandung, Jawa Barat 40161"
	if p.AddressText != want {
		t.Errorf("address_text = %q\ningin         %q", p.AddressText, want)
	}
	if p.UpdatedAt == nil {
		t.Error("updated_at kosong setelah disimpan")
	}

	// Menyimpan mengganti SELURUH isian: yang dikirim kosong menjadi kosong.
	p, err = f.profiles.Update(admin(org), businessprofile.Input{DisplayName: "Toko Baju", RegionCode: "32.73"})
	if err != nil {
		t.Fatalf("Update kedua: %v", err)
	}
	if p.Industry != "" || p.TaxID != "" || p.Postcode != "" || p.Address != "" {
		t.Errorf("isian lama tertinggal: %+v", p)
	}
	if p.Region == nil || p.Region.District != nil || p.Region.Village != nil || p.AddressText != "Kota Bandung, Jawa Barat" {
		t.Errorf("wilayah tingkat kabupaten/kota = %+v, %q", p.Region, p.AddressText)
	}

	// Pembaca biasa melihat hasil yang sama.
	got, err := f.profiles.Get(staff(org))
	if err != nil || got.DisplayName != "Toko Baju" {
		t.Errorf("Get = %+v, %v", got, err)
	}
}

func TestUpdateValidation(t *testing.T) {
	f := setup(t)
	ctx := admin(uuid.New())
	long := strings.Repeat("a", 501)

	for name, tc := range map[string]struct {
		in    businessprofile.Input
		field string
	}{
		"nama kosong":             {businessprofile.Input{DisplayName: "   "}, "display_name"},
		"nama terlalu panjang":    {businessprofile.Input{DisplayName: long}, "display_name"},
		"bidang terlalu panjang":  {businessprofile.Input{DisplayName: "A", Industry: long}, "industry"},
		"email salah":             {businessprofile.Input{DisplayName: "A", Email: "bukan email"}, "email"},
		"telepon berisi huruf":    {businessprofile.Input{DisplayName: "A", Phone: "nol delapan satu"}, "phone"},
		"telepon terlalu pendek":  {businessprofile.Input{DisplayName: "A", Phone: "123"}, "phone"},
		"jenis usaha tak dikenal": {businessprofile.Input{DisplayName: "A", BusinessType: "koperasi"}, "business_type"},
		"nama legal panjang":      {businessprofile.Input{DisplayName: "A", LegalName: long}, "legal_name"},
		"npwp kurang digit":       {businessprofile.Input{DisplayName: "A", TaxID: "12345"}, "tax_id"},
		"npwp berisi huruf":       {businessprofile.Input{DisplayName: "A", TaxID: "12345678901234AB"}, "tax_id"},
		"alamat terlalu panjang":  {businessprofile.Input{DisplayName: "A", Address: long}, "address"},
		"wilayah tak dikenal":     {businessprofile.Input{DisplayName: "A", RegionCode: "99.99"}, "region_code"},
		"wilayah hanya provinsi":  {businessprofile.Input{DisplayName: "A", RegionCode: "32"}, "region_code"},
		"wilayah bukan kode":      {businessprofile.Input{DisplayName: "A", RegionCode: "Bandung"}, "region_code"},
		"kode pos bukan 5 angka":  {businessprofile.Input{DisplayName: "A", Postcode: "4016"}, "postcode"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.profiles.Update(ctx, tc.in)
			if fields := fieldErrors(t, err); fields[tc.field] == "" {
				t.Errorf("tidak ada galat untuk %s: %v", tc.field, fields)
			}
		})
	}

	// Yang gagal validasi tidak menyimpan apa pun.
	if p, _ := f.profiles.Get(ctx); p.UpdatedAt != nil {
		t.Errorf("isian yang tidak sah tersimpan: %+v", p)
	}

	// Kode pos yang tidak cocok dengan wilayahnya tetap diterima.
	p, err := f.profiles.Update(ctx, businessprofile.Input{DisplayName: "A", RegionCode: "32.73.07.1001", Postcode: "12345"})
	if err != nil || p.Postcode != "12345" {
		t.Errorf("kode pos yang diketik = %q, %v", p.Postcode, err)
	}
}

// Izin ditegakkan service, dan galat dari pengait diteruskan apa adanya —
// produk yang menentukan bentuknya.
func TestOnlyManageMayChange(t *testing.T) {
	f := setup(t)
	org := uuid.New()

	if _, err := f.profiles.Update(staff(org), businessprofile.Input{DisplayName: "A"}); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("Update tanpa izin = %v", err)
	}
	if _, err := f.profiles.SetLogo(staff(org), bytes.NewReader(pngBytes(t))); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("SetLogo tanpa izin = %v", err)
	}
	if _, err := f.profiles.RemoveLogo(staff(org)); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("RemoveLogo tanpa izin = %v", err)
	}
	if _, err := f.profiles.Get(context.Background()); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Get tanpa sesi = %v", err)
	}
	if p, _ := f.profiles.Get(staff(org)); p.UpdatedAt != nil {
		t.Error("percobaan tanpa izin meninggalkan baris profil")
	}
}

func TestTenantIsolation(t *testing.T) {
	f := setup(t)
	a, b := uuid.New(), uuid.New()

	if _, err := f.profiles.Update(admin(a), businessprofile.Input{DisplayName: "Bisnis A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.profiles.SetLogo(admin(a), bytes.NewReader(pngBytes(t))); err != nil {
		t.Fatal(err)
	}

	if p, err := f.profiles.Get(admin(b)); err != nil || p.DisplayName != "" || p.Logo != nil {
		t.Errorf("organization B melihat profil A: %+v, %v", p, err)
	}
	if _, err := f.profiles.Update(admin(b), businessprofile.Input{DisplayName: "Bisnis B"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.profiles.RemoveLogo(admin(b)); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.profiles.Get(admin(a)); p.DisplayName != "Bisnis A" || p.Logo == nil {
		t.Errorf("profil A berubah oleh organization B: %+v", p)
	}
}

func TestLogo(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)

	// Logo boleh diunggah sebelum profil pertama kali disimpan.
	p, err := f.profiles.SetLogo(ctx, bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatalf("SetLogo: %v", err)
	}
	if p.Logo == nil || p.Logo.ContentType != "image/png" || p.Logo.Width != 32 {
		t.Fatalf("logo = %+v", p.Logo)
	}
	first := p.Logo.ID

	// Menyimpan profil tidak menyentuh logo.
	if p, err = f.profiles.Update(ctx, businessprofile.Input{DisplayName: "Toko"}); err != nil || p.Logo == nil || p.Logo.ID != first {
		t.Fatalf("logo setelah Update = %+v, %v", p.Logo, err)
	}

	// Mengganti logo: id baru, berkas lama terhapus.
	p, err = f.profiles.SetLogo(ctx, bytes.NewReader(pngBytes(t)))
	if err != nil || p.Logo == nil || p.Logo.ID == first {
		t.Fatalf("logo pengganti = %+v, %v", p.Logo, err)
	}
	if _, _, err := f.media.Open(context.Background(), first); err == nil {
		t.Error("berkas logo lama masih ada")
	}

	// Unggahan yang ditolak tidak mengubah logo yang ada.
	second := p.Logo.ID
	if _, err := f.profiles.SetLogo(ctx, strings.NewReader("<svg/>")); err == nil {
		t.Error("SVG diterima sebagai logo")
	}
	if p, _ := f.profiles.Get(ctx); p.Logo == nil || p.Logo.ID != second {
		t.Errorf("logo berubah oleh unggahan yang ditolak: %+v", p.Logo)
	}

	p, err = f.profiles.RemoveLogo(ctx)
	if err != nil || p.Logo != nil {
		t.Fatalf("RemoveLogo = %+v, %v", p.Logo, err)
	}
	if _, _, err := f.media.Open(context.Background(), second); err == nil {
		t.Error("berkas logo masih ada setelah dihapus")
	}
	// Menghapus logo yang tidak ada bukan galat.
	if _, err := f.profiles.RemoveLogo(ctx); err != nil {
		t.Errorf("RemoveLogo kedua: %v", err)
	}
}

func TestRoutes(t *testing.T) {
	f := setup(t)
	org := uuid.New()

	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", f.profiles.Routes()...)
	appkit.Register(mux, "", f.media.PublicRoutes()...)

	do := func(ctx context.Context, method, path string, body io.Reader) (int, businessprofile.Profile, string) {
		req := httptest.NewRequest(method, path, body).WithContext(ctx)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var p businessprofile.Profile
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		return rec.Code, p, rec.Body.String()
	}

	if code, p, _ := do(staff(org), http.MethodGet, "/v1/business-profile", nil); code != http.StatusOK || p.DisplayName != "" {
		t.Errorf("GET sebelum disimpan = %d, %+v", code, p)
	}

	body := `{"display_name":"Toko Baju","region_code":"32.73","phone":"0812-3456-7890"}`
	if code, _, _ := do(staff(org), http.MethodPut, "/v1/business-profile", strings.NewReader(body)); code != http.StatusForbidden {
		t.Errorf("PUT tanpa izin = %d, ingin 403", code)
	}
	code, p, _ := do(admin(org), http.MethodPut, "/v1/business-profile", strings.NewReader(body))
	if code != http.StatusOK || p.DisplayName != "Toko Baju" || p.Region == nil {
		t.Fatalf("PUT = %d, %+v", code, p)
	}

	// Field yang tidak dikenal ditolak, bukan diabaikan.
	code, _, raw := do(admin(org), http.MethodPut, "/v1/business-profile", strings.NewReader(`{"display_name":"A","nama":"B"}`))
	if code != http.StatusBadRequest || !strings.Contains(raw, `"nama"`) {
		t.Errorf("field tak dikenal = %d, %s", code, raw)
	}
	code, _, raw = do(admin(org), http.MethodPut, "/v1/business-profile", strings.NewReader(`{"display_name":""}`))
	if code != http.StatusBadRequest || !strings.Contains(raw, `"display_name"`) {
		t.Errorf("nama kosong = %d, %s", code, raw)
	}

	if code, _, _ := do(staff(org), http.MethodPut, "/v1/business-profile/logo", bytes.NewReader(pngBytes(t))); code != http.StatusForbidden {
		t.Errorf("unggah logo tanpa izin = %d, ingin 403", code)
	}
	code, p, _ = do(admin(org), http.MethodPut, "/v1/business-profile/logo", bytes.NewReader(pngBytes(t)))
	if code != http.StatusOK || p.Logo == nil {
		t.Fatalf("unggah logo = %d, %+v", code, p)
	}
	// Logo dibuka tanpa sesi, lewat URL yang dijawab API.
	if code, _, _ := do(context.Background(), http.MethodGet, p.Logo.URL, nil); code != http.StatusOK {
		t.Errorf("GET %s tanpa sesi = %d", p.Logo.URL, code)
	}

	code, _, raw = do(admin(org), http.MethodPut, "/v1/business-profile/logo", bytes.NewReader(make([]byte, 64<<10+100)))
	if code != http.StatusBadRequest || !strings.Contains(raw, "maksimal 64 KB") {
		t.Errorf("logo terlalu besar = %d, %s", code, raw)
	}

	if code, p, _ := do(admin(org), http.MethodDelete, "/v1/business-profile/logo", nil); code != http.StatusOK || p.Logo != nil {
		t.Errorf("hapus logo = %d, %+v", code, p.Logo)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	pool := testdb.New(t)
	m, err := media.New(pool, testdb.Hooks(), media.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := businessprofile.New(nil, m, testdb.Hooks()); err == nil {
		t.Error("New tanpa pool lolos")
	}
	if _, err := businessprofile.New(pool, nil, testdb.Hooks()); err == nil {
		t.Error("New tanpa media lolos")
	}
	if _, err := businessprofile.New(pool, m, appkit.Hooks{}); err == nil {
		t.Error("New tanpa pengait lolos")
	}
}
