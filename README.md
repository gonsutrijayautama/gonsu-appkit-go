# GONSU Appkit — modul standar produk (Go)

**Untuk tim yang membangun produk GONSU.**

Modul aplikasi yang sama di setiap produk, dipasang lewat `go get` alih-alih
disalin per produk:

| paket | isi |
|---|---|
| `businessprofile` | profil bisnis organization: nama, kontak, identitas legal, alamat, logo |
| `website` | halaman depan publik: tagline, layanan, kanal, SEO — dan penyisipannya ke HTML |
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
			case appkit.KindConflict:
				err = apperr.ConcurrentModification(e.Message)
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
sites, err := website.New(pool, profiles, files, hooks, website.Options{
	// organization halaman publik, yang dibuka tanpa sesi.
	PublicOrganization: func(*http.Request) (uuid.UUID, error) { return installationOrg, nil },
})
regionRoutes, err := regions.Routes(hooks)

// Di balik middleware sesi produk, di bawah /v1:
for _, rt := range slices.Concat(profiles.Routes(), sites.Routes(), regionRoutes) {
	api.Method(rt.Method, rt.Path, rt.Handler) // chi
}

// TANPA sesi, di akar situs — logo dan halaman depan tampil sebelum ada yang login:
for _, rt := range slices.Concat(files.PublicRoutes(), sites.PublicRoutes()) {
	r.Method(rt.Method, rt.Path, rt.Handler)
}
```

Halaman depan (`/`) hasil build frontend dilewatkan `sites.RenderHome(r, page)`
sebelum disajikan; lihat bagian Website.

Dengan `http.ServeMux` bawaan Go: `appkit.Register(mux, "/v1", routes...)`.

## Endpoint

Relatif terhadap akar API produk (`/v1`), di balik sesi:

| method | path | izin | keterangan |
|---|---|---|---|
| `GET` | `/business-profile` | sesi apa pun | profil organization ini; kosong bila belum disimpan |
| `PUT` | `/business-profile` | `settings.business.manage` | simpan SELURUH isian |
| `PUT` | `/business-profile/logo` | `settings.business.manage` | body: isi gambar PNG, JPEG, atau WebP |
| `DELETE` | `/business-profile/logo` | `settings.business.manage` | hapus logo |
| `GET` | `/website` | sesi apa pun | pengaturan halaman depan; bawaannya hanya pintu masuk |
| `PUT` | `/website` | `settings.website.manage` | simpan SELURUH isian |
| `PUT` | `/website/images/{slot}` | `settings.website.manage` | body: isi gambar; slot `about` atau `seo` |
| `DELETE` | `/website/images/{slot}` | `settings.website.manage` | hapus gambar |
| `GET` | `/regions?parent=<kode>` | sesi apa pun | anak langsung; tanpa `parent`: provinsi |
| `GET` | `/regions/search?q=&limit=` | sesi apa pun | cari kabupaten/kota, kecamatan, desa |

Tanpa sesi, di akar situs:

| method | path | keterangan |
|---|---|---|
| `GET` | `/media/{id}` | isi berkas; boleh disimpan peramban selamanya |
| `GET` | `/site.json` | tampilan publik halaman depan |

**Simpan-bersamaan.** `PUT /business-profile` dan `PUT /website` membawa
`version` yang dibaca dari `GET`. Bila datanya sudah diubah orang lain sejak
itu, jawabannya galat `KindConflict` dan tidak ada yang tersimpan. Mengganti
logo atau gambar tidak menaikkan `version`.

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
  "version": 3,
  "updated_at": "2026-10-02T03:04:05Z"
}
```

Body `PUT /business-profile` memuat sepuluh isian yang dapat diubah —
`display_name`, `industry`, `email`, `phone`, `business_type`, `legal_name`,
`tax_id`, `address`, `region_code`, `postcode` — dan `version`. Field lain
ditolak.

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

## Website

Halaman depan publik sebuah organization. Identitasnya (nama, logo, kontak,
alamat) dibaca dari profil bisnis; yang diatur di sini hanya yang khas halaman
depan.

```json
{
  "mode": "site",
  "tagline": "Pakaian rapi untuk setiap hari",
  "summary": "Toko pakaian keluarga di Bandung sejak 2010.",
  "about": { "text": "…", "image": null },
  "services": [{ "title": "Jahit ukuran", "description": "…", "icon": "wrench" }],
  "contact": { "hours": "Senin–Sabtu 09.00–17.00", "map_url": "https://…", "hide_address": false },
  "channels": { "whatsapp": "6281234567890", "instagram": "https://www.instagram.com/…", "facebook": "", "tiktok": "", "youtube": "", "linkedin": "" },
  "seo": { "title": "", "description": "", "image": null },
  "icons": ["package", "chart", "wrench", "…"],
  "version": 2,
  "updated_at": "2026-10-02T03:04:05Z"
}
```

- `mode`: `"signin"` (bawaan) hanya menampilkan pintu masuk; `"site"`
  menampilkan web perusahaan. Pada `"signin"`, tampilan publik hanya membawa
  nama, logo, dan judul.
- `services` maksimal 8; `icon` salah satu dari `icons`.
- `channels.whatsapp` disimpan sebagai digit berkode negara (nomor berawalan
  `0` dianggap nomor Indonesia). Kanal lain berupa alamat `https` di situs
  kanalnya.
- `contact.hide_address` menyembunyikan alamat jalan; kota tetap tampil.
- `seo` yang kosong diturunkan: judul dari nama bisnis dan tagline, deskripsi
  dari ringkasan, gambar dari logo.
- Gambar diatur lewat `PUT /website/images/{slot}`, tidak lewat `PUT /website`.
- Isinya disimpan sebagai satu dokumen JSON, jadi isian baru tidak butuh
  migrasi. Yang sengaja tidak ada: warna atau tema sendiri, skrip analytics,
  dan multi-bahasa.

**Tampilan publik** (`GET /site.json`, `Service.Public`) adalah gabungan profil
bisnis dan pengaturan ini yang sudah disaring untuk pengunjung. NPWP, nama
legal, dan jenis usaha tidak pernah ada di dalamnya. Ia disimpan di memori
selama `Options.CacheTTL` (bawaan 15 detik).

**Halaman depan.** `Service.RenderHome(r, page)` menyisipkan tampilan publik
ke HTML halaman depan: `<title>`, deskripsi, tag `og:` untuk pratinjau tautan,
dan datanya sendiri di `<script id="gonsu-site" type="application/json">`.
Frontend membaca elemen itu, sehingga halaman depan tidak memanggil API.
`RenderHome` tidak pernah gagal: bila datanya tidak terbaca, halaman
dikembalikan apa adanya — halaman depan adalah probe platform. Alamat aplikasi
untuk `og:image` diturunkan dari permintaan, tidak disimpan.

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

- **v0.2.0**
  - Baru: modul `website` (pengaturan, tampilan publik, `RenderHome`).
  - **Memutus:** `PUT /business-profile` kini wajib membawa `version`, dan
    jawaban profil memuat `version`. Simpan dengan `version` lama dijawab
    galat jenis baru `appkit.KindConflict` — tambahkan pemetaannya di
    `Hooks.WriteError` produk.
  - Migrasi baru: `00003` (kolom `version` profil bisnis) dan `00004`
    (`appkit_websites`).
- **v0.1.0** — `businessprofile`, `media`, dan `regions` pertama kali.

## Lisensi

Proprietary; lihat `LICENSE`. Repository ini publik supaya `go get` berjalan
tanpa kredensial, bukan supaya bebas dipakai siapa pun.
