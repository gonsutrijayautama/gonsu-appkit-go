package regions_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/regions"
)

func TestChildren(t *testing.T) {
	for _, tc := range []struct {
		parent, level, wantCode, wantName string
	}{
		{"", "province", "32", "Jawa Barat"},
		{"32", "regency", "32.73", "Kota Bandung"},
		{"32.73", "district", "32.73.07", "Sukajadi"},
		{"32.73.07", "village", "32.73.07.1001", "Pasteur"},
	} {
		items, ok := regions.Children(tc.parent)
		if !ok {
			t.Fatalf("Children(%q) tidak dikenal", tc.parent)
		}
		found := false
		for _, it := range items {
			if it.Level != tc.level {
				t.Fatalf("Children(%q) memuat level %q, ingin %q", tc.parent, it.Level, tc.level)
			}
			if it.Code == tc.wantCode {
				found = true
				if it.Name != tc.wantName {
					t.Errorf("nama %s = %q, ingin %q", it.Code, it.Name, tc.wantName)
				}
				if (it.PostalCode != "") != (tc.level == "village") {
					t.Errorf("kode pos %s = %q", it.Code, it.PostalCode)
				}
			}
		}
		if !found {
			t.Errorf("Children(%q) tidak memuat %s", tc.parent, tc.wantCode)
		}
	}

	// Desa tidak punya anak: daftar kosong, bukan galat.
	if items, ok := regions.Children("32.73.07.1001"); !ok || len(items) != 0 {
		t.Errorf("anak desa = %v, %v", items, ok)
	}
	for _, parent := range []string{"99", "32.99", "bukan-kode"} {
		if _, ok := regions.Children(parent); ok {
			t.Errorf("Children(%q) dikenal", parent)
		}
	}
}

func TestSearch(t *testing.T) {
	items := regions.Search("pasteur", 5)
	if len(items) == 0 || len(items) > 5 {
		t.Fatalf("hasil = %d", len(items))
	}
	found := false
	for _, it := range items {
		if it.Level == "province" {
			t.Errorf("provinsi ikut dicari: %+v", it)
		}
		if it.Code == "32.73.07.1001" {
			found = true
			if it.Label != "Desa Pasteur, Kecamatan Sukajadi, Kota Bandung, Jawa Barat" || it.PostalCode != "40161" {
				t.Errorf("hasil = %+v", it)
			}
		}
	}
	if !found {
		t.Error("Pasteur, Sukajadi, Kota Bandung tidak ditemukan")
	}

	// Query pendek mengembalikan daftar kosong, bukan ribuan baris.
	if got := regions.Search("ba", 0); got == nil || len(got) != 0 {
		t.Errorf("query dua huruf = %v", got)
	}
	// Batas atas tetap berlaku walau klien meminta lebih.
	if got := regions.Search("kota", 10000); len(got) > 50 {
		t.Errorf("hasil = %d, melewati batas", len(got))
	}
}

func TestRoutes(t *testing.T) {
	routes, err := regions.Routes(testdb.Hooks())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", routes...)

	get := func(path string) (int, []regions.Item) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body struct {
			Data []regions.Item `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Data
	}

	if code, data := get("/v1/regions"); code != http.StatusOK || len(data) < 34 {
		t.Errorf("GET /v1/regions = %d, %d provinsi", code, len(data))
	}
	if code, data := get("/v1/regions?parent=32"); code != http.StatusOK || len(data) == 0 {
		t.Errorf("GET /v1/regions?parent=32 = %d, %d baris", code, len(data))
	}
	if code, _ := get("/v1/regions?parent=99"); code != http.StatusBadRequest {
		t.Errorf("parent tidak dikenal = %d, ingin 400", code)
	}
	if code, data := get("/v1/regions/search?q=sukajadi&limit=3"); code != http.StatusOK || len(data) == 0 || len(data) > 3 {
		t.Errorf("pencarian = %d, %d baris", code, len(data))
	}
	if _, err := regions.Routes(appkit.Hooks{}); err == nil {
		t.Error("Routes tanpa pengait lolos")
	}
}
