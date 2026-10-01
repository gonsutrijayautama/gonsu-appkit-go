// Package regions menyajikan wilayah administratif Indonesia — provinsi,
// kabupaten/kota, kecamatan, dan desa/kelurahan beserta kode posnya — untuk
// pemilih alamat di formulir.
//
// Datanya dari github.com/aliziodev/go-indonesia-regions, tertanam di binary:
// tidak ada tabel, tidak ada panggilan jaringan. Platform GONSU One memakai
// library dan versi yang sama, sehingga nama kabupaten/kota di produk sama
// persis dengan yang tercetak di faktur platform.
//
// Nama disajikan apa adanya dari data resmi dan tidak pernah diubah
// kapitalisasinya.
package regions

import (
	"net/http"
	"strconv"
	"strings"

	wilayah "github.com/aliziodev/go-indonesia-regions"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// Item adalah satu wilayah di API.
type Item struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Level: "province", "regency", "district", atau "village".
	Level string `json:"level"`
	// PostalCode hanya terisi untuk desa/kelurahan.
	PostalCode string `json:"postal_code,omitempty"`
	// Label adalah nama lengkap sampai provinsi; hanya terisi di hasil
	// pencarian, tempat nama saja tidak cukup membedakan.
	Label string `json:"label,omitempty"`
}

// Children mengembalikan wilayah satu tingkat di bawah parent, urut kode.
// parent kosong mengembalikan seluruh provinsi; kode desa mengembalikan
// daftar kosong. ok false bila parent bukan kode wilayah yang ada.
func Children(parent string) (items []Item, ok bool) {
	if parent == "" {
		for _, p := range wilayah.Provinces() {
			items = append(items, Item{Code: p.Code, Name: p.Name, Level: wilayah.LevelProvince.String()})
		}
		return items, true
	}
	region, found := wilayah.Find(parent)
	if !found {
		return nil, false
	}
	items = []Item{}
	switch region.Level {
	case wilayah.LevelProvince:
		for _, r := range wilayah.Regencies(parent) {
			items = append(items, Item{Code: r.Code, Name: r.Name, Level: wilayah.LevelRegency.String()})
		}
	case wilayah.LevelRegency:
		for _, d := range wilayah.Districts(parent) {
			items = append(items, Item{Code: d.Code, Name: d.Name, Level: wilayah.LevelDistrict.String()})
		}
	case wilayah.LevelDistrict:
		for _, v := range wilayah.Villages(parent) {
			items = append(items, Item{Code: v.Code, Name: v.Name, Level: wilayah.LevelVillage.String(), PostalCode: v.PostalCode})
		}
	}
	return items, true
}

const (
	// minQuery: di bawah ini hasilnya ribuan baris yang tidak berguna.
	minQuery     = 3
	defaultLimit = 20
	maxLimit     = 50
)

// Search mencari kabupaten/kota, kecamatan, dan desa/kelurahan yang namanya
// memuat query. Provinsi tidak dicari: alamat minimal sampai kabupaten/kota.
// Query yang lebih pendek dari tiga huruf mengembalikan daftar kosong.
func Search(query string, limit int) []Item {
	query = strings.TrimSpace(query)
	items := []Item{}
	if len([]rune(query)) < minQuery {
		return items
	}
	if limit < 1 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	found := wilayah.Search(query, wilayah.Limit(limit),
		wilayah.AtLevel(wilayah.LevelRegency, wilayah.LevelDistrict, wilayah.LevelVillage))
	for _, r := range found {
		it := Item{Code: r.Code, Name: r.Name, Level: r.Level.String()}
		if addr, ok := wilayah.Resolve(r.Code); ok {
			it.Label = addr.Format(wilayah.WithoutPostalCode())
			it.PostalCode = addr.PostalCode()
		}
		items = append(items, it)
	}
	return items
}

// Routes adalah endpoint wilayah, relatif terhadap akar API produk (`/v1`):
//
//	GET /regions?parent=<kode>      anak langsung; tanpa parent: provinsi
//	GET /regions/search?q=&limit=   pencarian nama
//
// Datanya publik, tetapi pasang di balik sesi seperti endpoint API lain:
// tidak ada alasan menyajikannya ke pengunjung tanpa akun.
func Routes(hooks appkit.Hooks) ([]appkit.Route, error) {
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	h := handler{writeError: hooks.WriteError}
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/regions", Handler: h.children},
		{Method: http.MethodGet, Path: "/regions/search", Handler: h.search},
	}, nil
}

type handler struct {
	writeError func(w http.ResponseWriter, r *http.Request, err error)
}

func (h handler) children(w http.ResponseWriter, r *http.Request) {
	items, ok := Children(r.URL.Query().Get("parent"))
	if !ok {
		h.writeError(w, r, appkit.Validation("Wilayah tidak dikenal.",
			appkit.FieldError{Field: "parent", Message: "Kode wilayah tidak dikenal."}))
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": items})
}

func (h handler) search(w http.ResponseWriter, r *http.Request) {
	// Nilai limit yang tidak valid jatuh ke bawaan: ini parameter tampilan.
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	httpjson.Write(w, http.StatusOK, map[string]any{"data": Search(r.URL.Query().Get("q"), limit)})
}
