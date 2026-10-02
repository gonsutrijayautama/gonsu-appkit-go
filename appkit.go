// Package appkit adalah akar library modul standar produk GONSU: profil
// bisnis, media, dan data wilayah yang sama di setiap produk.
//
// Library ini BUKAN bagian SDK GONSU One. SDK membawa kontrak dengan platform
// (lisensi, login); library ini membawa modul aplikasi yang hidup di dalam
// produk, dengan tabel dan endpoint-nya sendiri, dan tidak pernah memanggil
// platform.
//
// Pembagian tugasnya dengan produk:
//
//   - library memiliki tabelnya (awalan `appkit_`) dan migrasinya (Migrate);
//   - library tidak meng-import kode produk. Produk menyerahkan PENGAIT
//     (Hooks): cara membaca organization, memeriksa izin, dan menulis galat;
//   - route diserahkan sebagai daftar (Route), sehingga produk memasangnya di
//     router apa pun, di balik middleware sesi dan hak pakainya sendiri.
package appkit

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
)

// Permission adalah nama kemampuan yang diminta sebuah modul. Nilainya
// didefinisikan modul masing-masing (mis. businessprofile.Manage); siapa yang
// memegangnya diputuskan produk lewat Hooks.Authorize.
type Permission string

// Hooks adalah yang diserahkan produk ke setiap modul. Organization,
// Authorize, dan WriteError wajib; User wajib hanya untuk modul yang
// menyebutnya.
type Hooks struct {
	// Organization mengembalikan organization request ini, dari sesi yang
	// sudah diperiksa produk. Modul tidak pernah membaca organization dari
	// body, query, atau environment.
	Organization func(ctx context.Context) (uuid.UUID, error)

	// Authorize memeriksa apakah pengguna request ini memegang perm. Galat
	// yang dikembalikannya diteruskan apa adanya ke WriteError, jadi produk
	// bebas memakai tipe galatnya sendiri. Permission yang tidak dikenal
	// produk harus DITOLAK, bukan diloloskan.
	Authorize func(ctx context.Context, perm Permission) error

	// WriteError menulis jawaban galat dengan envelope milik produk. Galat
	// dari modul bertipe *Error; galat dari pengait diteruskan apa adanya;
	// selain itu galat tak terduga yang rinciannya tidak boleh sampai ke
	// pengguna.
	WriteError func(w http.ResponseWriter, r *http.Request, err error)

	// User mengembalikan id pengguna request ini di dalam produk, dari sesi
	// yang sudah diperiksa produk. Dipakai modul yang mencatat siapa pelaku
	// sebuah perubahan (package roles); modul lain tidak membutuhkannya, jadi
	// Validate tidak mewajibkannya.
	User func(ctx context.Context) (uuid.UUID, error)
}

// Validate memastikan ketiga pengait wajib terisi. Dipanggil konstruktor modul:
// pengait yang kosong harus gagal saat start, bukan saat permintaan pertama.
func (h Hooks) Validate() error {
	switch {
	case h.Organization == nil:
		return errors.New("appkit: Hooks.Organization wajib diisi")
	case h.Authorize == nil:
		return errors.New("appkit: Hooks.Authorize wajib diisi")
	case h.WriteError == nil:
		return errors.New("appkit: Hooks.WriteError wajib diisi")
	}
	return nil
}

// Route adalah satu endpoint modul.
//
// Path ditulis dengan sintaks `{nama}` yang dipahami net/http maupun chi, dan
// relatif terhadap tempat memasangnya: akar API produk (`/v1`) untuk Routes,
// akar situs untuk PublicRoutes. Handler tidak membaca parameter path lewat
// router, sehingga tidak terikat pada router tertentu.
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

// Register memasang routes ke ServeMux bawaan Go di bawah prefix (mis. "/v1";
// kosong untuk akar situs). Produk yang memakai router lain cukup mengulang
// daftarnya sendiri.
func Register(mux *http.ServeMux, prefix string, routes ...Route) {
	for _, rt := range routes {
		mux.Handle(rt.Method+" "+prefix+rt.Path, rt.Handler)
	}
}
