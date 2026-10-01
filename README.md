# GONSU Appkit — modul standar produk (Go)

**Untuk tim yang membangun produk GONSU.**

Modul aplikasi yang sama di setiap produk, dipasang lewat `go get` alih-alih
disalin per produk:

| paket | isi |
|---|---|
| `businessprofile` | profil bisnis organization: nama, kontak, identitas legal, alamat, logo |
| `media` | berkas publik (logo, gambar), dengan penyimpanan yang dapat diganti |
| `regions` | wilayah Indonesia sampai desa beserta kode pos, untuk pemilih alamat |

Project hasil `gonsu new` sudah memasangnya. Halaman frontend-nya ada di
template `gonsu-cli`, bukan di sini.

Library ini **bukan** SDK GONSU One. SDK (`gonsu-one-sdk-go`) membawa kontrak
dengan platform: lisensi dan login. Library ini tidak pernah memanggil
platform; isinya modul yang hidup di dalam produk, dengan tabel dan
endpoint-nya sendiri.

> **Status: v0.x.** Bentuknya masih boleh berubah sampai v1.0.0. Sejak v1,
> kontraknya hanya boleh bertambah.

## Memasang

```
go get github.com/gonsutrijayautama/gonsu-appkit-go
```

Butuh PostgreSQL dan `pgxpool`. Tiga langkah:

### 1. Migrasi saat start

Library memiliki tabelnya sendiri (awalan `appkit_`) dan mencatat migrasinya
di `appkit_schema_migrations`, terpisah dari migrasi produk. Panggil
**sebelum** migrasi produk, supaya tabel produk boleh merujuk tabel library:

```go
if err := appkit.Migrate(ctx, pool, logger); err != nil {
	return err // jangan melayani di atas skema setengah jadi
}
```

### 2. Pengait

Library tidak meng-import kode produk. Produk menyerahkan tiga pengait:

```go
hooks := appkit.Hooks{
	// organization request ini, dari sesi yang sudah diperiksa produk.
	Organization: tenant.OrganizationID,

	// Pemetaan izin library ke izin produk. Yang tidak dikenal DITOLAK.
	Authorize: func(ctx context.Context, perm appkit.Permission) error {
		switch perm {
		case businessprofile.Manage:
			_, err := authn.Check(ctx, authz.SettingsBusinessManage)
			return err
		}
		return apperr.PermissionDenied("Anda tidak memiliki izin untuk tindakan ini.")
	},

	// Galat modul (*appkit.Error) diterjemahkan ke envelope produk; galat
	// lain — termasuk yang dikembalikan Authorize — diteruskan apa adanya.
	WriteError: func(w http.ResponseWriter, r *http.Request, err error) {
		if e, ok := errors.AsType[*appkit.Error](err); ok {
			switch e.Kind {
			case appkit.KindValidation:
				err = apperr.Validation(e.Message, fields(e.Fields)...)
			case appkit.KindNotFound:
				err = apperr.NotFound(e.Message)
			}
		}
		httpx.WriteError(w, r, logger, err)
	},
}
```

### 3. Route

Setiap modul menyerahkan daftar route, sehingga dapat dipasang di router apa
pun. Path-nya memakai sintaks `{nama}` yang dipahami `net/http` dan chi.

```go
files, err := media.New(pool, hooks, media.Options{})
profiles, err := businessprofile.New(pool, files, hooks)
regionRoutes, err := regions.Routes(hooks)

// Di balik middleware sesi produk, di bawah /v1:
for _, rt := range slices.Concat(profiles.Routes(), regionRoutes) {
	api.Method(rt.Method, rt.Path, rt.Handler) // chi
}

// TANPA sesi, di akar situs — logo tampil sebelum ada yang login:
for _, rt := range files.PublicRoutes() {
	r.Method(rt.Method, rt.Path, rt.Handler)
}
```

Dengan `http.ServeMux` bawaan Go: `appkit.Register(mux, "/v1", routes...)`.

## Endpoint

Relatif terhadap akar API produk (`/v1`), di balik sesi:

| method | path | izin | keterangan |
|---|---|---|---|
| `GET` | `/business-profile` | sesi apa pun | profil organization ini; kosong bila belum disimpan |
| `PUT` | `/business-profile` | `settings.business.manage` | simpan SELURUH isian |
| `PUT` | `/business-profile/logo` | `settings.business.manage` | body: isi gambar PNG, JPEG, atau WebP |
| `DELETE` | `/business-profile/logo` | `settings.business.manage` | hapus logo |
| `GET` | `/regions?parent=<kode>` | sesi apa pun | anak langsung; tanpa `parent`: provinsi |
| `GET` | `/regions/search?q=&limit=` | sesi apa pun | cari kabupaten/kota, kecamatan, desa |

Tanpa sesi, di akar situs:

| method | path | keterangan |
|---|---|---|
| `GET` | `/media/{id}` | isi berkas; boleh disimpan peramban selamanya |

### Profil bisnis

```json
{
  "display_name": "Toko Baju Sejahtera",
  "industry": "Ritel pakaian",
  "email": "halo@tokobaju.example",
  "phone": "(022) 123-4567",
  "business_type": "company",
  "legal_name": "PT Baju Sejahtera Makmur",
  "tax_id": "0012345678901000",
  "address": "Jl. Pasteur No. 10",
  "region_code": "32.73.07.1001",
  "region": {
    "province": { "code": "32", "name": "Jawa Barat" },
    "regency": { "code": "32.73", "name": "Kota Bandung" },
    "district": { "code": "32.73.07", "name": "Sukajadi" },
    "village": { "code": "32.73.07.1001", "name": "Pasteur" },
    "label": "Desa Pasteur, Kecamatan Sukajadi, Kota Bandung, Jawa Barat"
  },
  "postcode": "40161",
  "address_text": "Jl. Pasteur No. 10, Desa Pasteur, Kecamatan Sukajadi, Kota Bandung, Jawa Barat 40161",
  "logo": { "id": "…", "url": "/media/…", "content_type": "image/png", "size": 1234, "width": 256, "height": 256, "created_at": "…" },
  "updated_at": "2026-10-02T03:04:05Z"
}
```

Body `PUT /business-profile` memuat sepuluh isian yang dapat diubah:
`display_name`, `industry`, `email`, `phone`, `business_type`, `legal_name`,
`tax_id`, `address`, `region_code`, `postcode`. Field lain ditolak.

- `display_name` wajib. Isian lain boleh kosong (`""`, bukan `null`).
- `business_type`: `""`, `"individual"`, atau `"company"`.
- `tax_id` disimpan 16 digit tanpa pemisah. NPWP lama 15 digit mendapat
  awalan `0`.
- `region_code` adalah kode Kemendagri, minimal kabupaten/kota (`32.73`),
  boleh sampai desa (`32.73.07.1001`). `region` diturunkan saat dibaca.
- `postcode` yang kosong diisi dari desa terpilih. Yang diketik hanya
  diperiksa bentuknya (5 angka).

Makna `business_type`, `legal_name`, `tax_id`, alamat, kode pos, email, dan
telepon sama dengan profil penagihan platform GONSU One, dan
`region.regency.name` sama persis dengan `billing_city` di sana.

## Media

- Setiap berkas dapat dibaca **siapa pun** yang mengetahui URL-nya. Jangan
  simpan berkas yang butuh izin di sini.
- Jenisnya dikenali dari isi berkas. SVG ditolak.
- Batas ukuran bawaan 2 MB (`media.Options.MaxBytes`).
- Isi berkas disimpan lewat antarmuka `media.Store`. Bawaannya `media.DBStore`
  (tabel `appkit_media_blobs`), karena pemasangan cloud GONSU belum
  menyediakan disk maupun object storage untuk produk. Akibatnya isi berkas
  ikut masuk backup database.
- Mengganti `Store` tidak mengubah skema, API, maupun URL berkas.

## Mengembangkan library ini

```
make test   # seluruh test terhadap PostgreSQL lokal (docker compose)
make lint   # go vet dan gofmt
```

Aturan kontribusi ada di `AGENTS.md`.

## Perubahan

- **v0.1.0** — `businessprofile`, `media`, dan `regions` pertama kali.

## Lisensi

Proprietary; lihat `LICENSE`. Repository ini publik supaya `go get` berjalan
tanpa kredensial, bukan supaya bebas dipakai siapa pun.
