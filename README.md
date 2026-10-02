# GONSU Appkit — modul standar produk (Go)

**Untuk tim yang membangun produk GONSU.**

Modul aplikasi yang sama di setiap produk, dipasang lewat `go get` alih-alih
disalin per produk:

| paket | isi |
|---|---|
| `businessprofile` | profil bisnis organization: nama, kontak, identitas legal, alamat, logo |
| `website` | identitas halaman depan publik: mode, tagline, kontak, kanal, pratinjau tautan — dan penyisipannya ke HTML |
| `media` | berkas PUBLIK (logo, gambar): di database atau object storage S3/R2, dengan kuota per organization |
| `regions` | wilayah Indonesia sampai desa beserta kode pos, untuk pemilih alamat |
| `roles` | role per organization: role bawaan di kode, role buatan dari layar, dan izin yang berlaku bagi pemegangnya |
| `users` | pengguna per organization: siapa berhak masuk dan dengan role apa, pemberian akses, dan batas pengguna |
| `audit` | jejak audit per organization: siapa melakukan apa, kapan, dan dari alamat mana |
| `idempotency` | mutasi yang diulang klien tidak menghasilkan dokumen ganda |
| `numbering` | nomor dokumen apa pun: bentuk per organization, tanpa nomor kembar dan tanpa nomor lompat |
| `attachments` | berkas privat (lampiran dokumen): hanya terbaca lewat dokumen pemiliknya |

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

Library tidak meng-import kode produk. Produk menyerahkan pengait: tiga yang
wajib bagi setiap modul, dan `User` yang diminta jejak audit — jadi wajib bagi
produk yang memasang `businessprofile`, `website`, `roles`, atau `users`.

```go
hooks := appkit.Hooks{
	// organization request ini, dari sesi yang sudah diperiksa produk.
	Organization: tenant.OrganizationID,

	// id pengguna request ini di dalam produk: pelaku yang dicatat jejak audit.
	User: authn.UserID,

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
			case appkit.KindQuotaExceeded:
				// Kuota paket penuh: tawarkan naik paket, bukan galat isian.
				err = apperr.QuotaExceeded(e.Message)
			case appkit.KindIdempotencyConflict:
				err = apperr.IdempotencyKeyConflict(e.Message)
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
// Jejak audit lebih dulu: modul yang mengubah data mencatat lewat ini.
trail, err := audit.New(pool, hooks, audit.Options{ClientAddr: httpx.ClientAddr})
profiles, err := businessprofile.New(pool, files, trail, hooks)
sites, err := website.New(pool, profiles, files, trail, hooks, website.Options{
	// organization halaman publik, yang dibuka tanpa sesi.
	PublicOrganization: func(*http.Request) (uuid.UUID, error) { return installationOrg, nil },
})
regionRoutes, err := regions.Routes(hooks)

// Di balik middleware sesi produk, di bawah /v1:
for _, rt := range slices.Concat(profiles.Routes(), sites.Routes(), trail.Routes(), regionRoutes) {
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
| `PUT` | `/website/images/{slot}` | `settings.website.manage` | body: isi gambar; slot `seo` |
| `DELETE` | `/website/images/{slot}` | `settings.website.manage` | hapus gambar |
| `GET` | `/regions?parent=<kode>` | sesi apa pun | anak langsung; tanpa `parent`: provinsi |
| `GET` | `/regions/search?q=&limit=` | sesi apa pun | cari kabupaten/kota, kecamatan, desa |
| `GET` | `/roles` | `settings.roles.manage` | role, katalog izin, dan batas role buatan |
| `POST` | `/roles` | `settings.roles.manage` | buat role buatan; menjawab `201` |
| `PUT` | `/roles/{key}` | `settings.roles.manage` | simpan SELURUH isian role buatan |
| `DELETE` | `/roles/{key}` | `settings.roles.manage` | hapus role buatan; menjawab `204` |
| `GET` | `/users` | `settings.users.manage` | pengguna, batas pengguna, kesiapan pemberian akses, dan role |
| `POST` | `/users` | `settings.users.manage` | beri akses lewat email; menjawab `201` |
| `PATCH` | `/users/{id}` | `settings.users.manage` | ubah role atau status |
| `GET` | `/audit-events?category=&before=&limit=` | `settings.audit.view` | jejak audit, terbaru dulu |
| `GET` | `/document-numbering` | `settings.numbering.manage` | skema penomoran tiap jenis dokumen, dan daftar token |
| `PUT` | `/document-numbering/{type}` | `settings.numbering.manage` | simpan pola dan kebijakan reset satu jenis dokumen |

Tanpa sesi, di akar situs:

| method | path | keterangan |
|---|---|---|
| `GET` | `/media/{id}` | isi berkas; boleh disimpan peramban selamanya |
| `GET` | `/site.json` | tampilan publik halaman depan |

**Simpan-bersamaan.** `PUT /business-profile`, `PUT /website`,
`PUT /roles/{key}`, dan `PUT /document-numbering/{type}` membawa `version`
yang dibaca dari `GET`. Bila datanya
sudah diubah orang lain sejak itu, jawabannya galat `KindConflict` dan tidak
ada yang tersimpan. Mengganti logo atau gambar tidak menaikkan `version`.

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

Identitas halaman depan publik sebuah organization: apakah ia punya halaman
publik, dan bagaimana ia memperkenalkan diri. Nama, logo, kontak, dan alamat
dibaca dari profil bisnis; yang diatur di sini hanya yang khas halaman depan.

**Isi halaman bukan urusan modul ini.** Bagian "tentang", daftar layanan, dan
susunan halaman lainnya milik penyusun halaman, yang membaca identitas di sini
sebagai sumber datanya. Modul ini tidak menyimpan teks panjang, daftar, maupun
gambar bagian halaman.

```json
{
  "mode": "site",
  "tagline": "Pakaian rapi untuk setiap hari",
  "summary": "Toko pakaian keluarga di Bandung sejak 2010.",
  "contact": { "hours": "Senin–Sabtu 09.00–17.00", "map_url": "https://…", "hide_address": false },
  "channels": { "whatsapp": "6281234567890", "instagram": "https://www.instagram.com/…", "facebook": "", "tiktok": "", "youtube": "", "linkedin": "" },
  "seo": { "title": "", "description": "", "image": null },
  "version": 2,
  "updated_at": "2026-10-02T03:04:05Z"
}
```

- `mode` menjawab satu pertanyaan: apakah organization ini punya halaman
  publik. `"signin"` (bawaan) hanya menampilkan pintu masuk, dan tampilan
  publiknya hanya membawa nama, logo, dan judul. `"site"` menampilkan halaman
  publik beserta pintu masuk. Mode menyebut apa yang didapat pengunjung, bukan
  alat yang menyusun halamannya: halaman dari penyusun halaman tampil di mode
  yang sama, jadi mode tidak bertambah saat penyusun halaman ada.
- `channels.whatsapp` disimpan sebagai digit berkode negara (nomor berawalan
  `0` dianggap nomor Indonesia). Kanal lain berupa alamat `https` di situs
  kanalnya.
- `contact.hide_address` menyembunyikan alamat jalan; kota tetap tampil.
- `seo` yang kosong diturunkan: judul dari nama bisnis dan tagline, deskripsi
  dari ringkasan, gambar dari logo.
- Gambar pratinjau diatur lewat `PUT /website/images/seo`, tidak lewat
  `PUT /website`.
- Isinya disimpan sebagai satu dokumen JSON, jadi isian baru tidak butuh
  migrasi. Yang sengaja tidak ada: isi halaman, warna atau tema sendiri,
  skrip analytics, dan multi-bahasa.

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

## Role

Role adalah kumpulan izin bernama. Pembagiannya:

| | tempatnya | siapa yang mengubah |
|---|---|---|
| izin | kode produk (`roles.Options.Permissions`) | rilis produk |
| role bawaan | kode produk (`roles.Options.Builtins`) | rilis produk |
| role buatan | tabel `appkit_roles`, per organization | pemegang `settings.roles.manage` |
| siapa memegang role apa | modul `users` (tabel `appkit_users`), yang menyimpan `key` role | pemegang `settings.users.manage` |

`roles` sendiri tidak tahu siapa memegang role apa; ia bertanya lewat dua opsi
yang diisi dari modul `users`. Produk yang menyimpan penggunanya sendiri
mengisi keduanya dari tabelnya, dan menanggung sendiri tiga hal yang dijaga
`users`: memeriksa role saat memberikannya, minimal satu administrator aktif,
dan batas pengguna.

### Memasang

```go
var (
	access *roles.Service
	people *users.Service // diisi di bagian Pengguna
)

hooks := appkit.Hooks{
	Organization: tenant.OrganizationID,
	User:         authn.UserID,
	// Satu jalur untuk izin produk dan izin modul library: yang tidak
	// dipegang role penggunanya DITOLAK.
	Authorize: func(ctx context.Context, perm appkit.Permission) error {
		p, err := authn.FromContext(ctx)
		if err != nil {
			return err
		}
		ok, err := access.Can(ctx, p.OrganizationID, p.Role, perm)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.PermissionDenied("Anda tidak memiliki izin untuk tindakan ini.")
		}
		return nil
	},
	WriteError: writeError,
}

trail, err := audit.New(pool, hooks, audit.Options{ClientAddr: httpx.ClientAddr})
access, err = roles.New(pool, trail, hooks, roles.Options{
	Permissions: []roles.Definition{
		{Name: "settings.users.manage", Group: "Pengaturan", Label: "Mengelola pengguna", Sensitive: true},
		{Name: roles.Manage, Group: "Pengaturan", Label: "Mengelola role", Sensitive: true},
		{Name: audit.View, Group: "Pengaturan", Label: "Melihat riwayat aktivitas", Sensitive: true},
		{Name: businessprofile.Manage, Group: "Pengaturan", Label: "Mengubah profil bisnis"},
		{Name: "notes.read", Group: "Catatan", Label: "Melihat catatan"},
		{Name: "notes.write", Group: "Catatan", Label: "Mengubah catatan"},
		{Name: "portal.view", Group: "Portal", Label: "Melihat pesanan sendiri", Audience: roles.AudienceExternal},
	},
	Builtins: []roles.Builtin{
		{Key: "administrator", Name: "Administrator", Administrator: true},
		{Key: "staff", Name: "Staf", Permissions: []appkit.Permission{"notes.read", "notes.write"}},
		{Key: "customer", Name: "Customer", Audience: roles.AudienceExternal, Permissions: []appkit.Permission{"portal.view"}},
	},
	// Hak pakai paket. Yang tidak dibawa paket dijawab false.
	CustomEnabled: func(ctx context.Context, org uuid.UUID) (bool, error) {
		return license.Feature(ctx, entitlement.RolesCustom), nil
	},
	// Keduanya dari modul users. Penutupnya ada karena roles dan users saling
	// membutuhkan: `people` baru terisi sesudah roles.New.
	UserCounts: func(ctx context.Context, org uuid.UUID) (map[string]int, error) {
		return people.CountByRole(ctx, org)
	},
	LockAssignments: func(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
		return people.LockGrants(ctx, tx, org)
	},
})
```

`roles.New` gagal saat start bila susunannya melanggar pagar di bawah, jadi
salah susun tidak pernah sampai ke pengguna. Route-nya (`access.Routes()`)
dipasang di balik sesi seperti modul lain.

Fungsi untuk produk, semuanya dengan `org` eksplisit dan tanpa sesi, jadi
dapat dipakai juga oleh perintah operator dan bootstrap pemilik:

```go
func (s *Service) PermissionsOf(ctx context.Context, org uuid.UUID, key string) ([]appkit.Permission, error)
func (s *Service) Can(ctx context.Context, org uuid.UUID, key string, perm appkit.Permission) (bool, error)
func (s *Service) Get(ctx context.Context, org uuid.UUID, key string) (Role, error)
func (s *Service) All(ctx context.Context, org uuid.UUID) ([]Role, error)
```

- `PermissionsOf` mengembalikan izin yang berlaku bagi pemegang role itu;
  `Can` memeriksa satu izin. **Role bawaan dijawab dari memori**, tanpa
  menyentuh database dan tanpa kemungkinan gagal; role buatan dengan satu
  query. Produk yang memeriksa banyak izin per permintaan memanggil
  `PermissionsOf` sekali di middleware sesinya.
- Role yang tidak ada, role milik organization lain, dan role yang sudah
  dihapus dijawab **tanpa izin apa pun**, bukan galat.
- Saat memberikan role ke pengguna, produk memanggil `Get`: tolak bila tidak
  ditemukan, dan cocokkan `audience` dengan jenis penggunanya. `All` mengisi
  pilihan dan nama role di layar pengguna.

### Pagar

- **Izin ada di kode.** Yang dapat disusun dari layar hanya role. Izin yang
  hanya ada di katalog tetapi tidak diperiksa kode mana pun tidak menjaga apa
  pun; menjaganya tugas test produk.
- **Role bawaan tidak dapat diubah atau dihapus.** Tepat satu bertanda
  `Administrator`: ia memegang seluruh izin internal, termasuk yang
  ditambahkan rilis berikutnya.
- **Izin `Sensitive` hanya dipegang administrator bawaan.** Tidak dapat
  dicentang ke role buatan maupun dipasang ke role bawaan lain. Tandai begitu
  izin mengelola pengguna, mengelola role, dan langganan.
  `settings.roles.manage` wajib ada di katalog dan wajib `Sensitive`.
- **Satu role satu audiens.** `internal` untuk staf; `external` untuk orang
  di luar organization, misalnya pelanggannya. Role `external` hanya berisi
  izin `external`, dan sebaliknya; audiens tidak berubah setelah role dibuat.
  Kode yang memeriksa izin `external` wajib membatasi datanya ke orang itu,
  dari sesi, tidak pernah dari body.
- **Audiens ketiga, `machine`, hanya untuk izin.** Ia untuk program yang
  masuk dengan token, bukan orang: tidak ada role beraudiens ini dan tidak
  ada role yang memegang izinnya. Modul token belum ada; tempatnya disiapkan
  supaya token kelak mencentang izin dari katalog yang sama. Izin `Sensitive`
  wajib `internal`, jadi tidak pernah dapat diberikan ke token.
- **Izin baru tidak masuk sendiri ke role buatan.** Izin yang dihapus dari
  katalog, yang kini `Sensitive`, atau yang berpindah audiens berhenti
  berlaku di role buatan tanpa migrasi: yang tersimpan disaring saat dibaca.
- **Role buatan adalah fitur paket, dan yang dijual adalah kemampuan
  menyusunnya.** Bila `CustomEnabled` menjawab false, role buatan tidak dapat
  dibuat atau diubah (galat `KindQuotaExceeded`); menghapusnya tetap boleh.
  Role buatan yang **sudah ada tetap berlaku**: organization yang turun paket
  tidak boleh mendapati stafnya terkunci seketika, sama seperti kuota
  penyimpanan yang penuh tidak menghapus berkas yang sudah ada. Karena itu
  `CustomEnabled` hanya dipanggil saat menyusun role, tidak pernah saat
  memeriksa izin: hak pakai yang gagal dibaca tidak dapat mengunci siapa pun.
- **Paling banyak 30 role buatan per organization** (`Options.MaxCustom`).
  Ini pengaman, bukan batas yang dijual; batasnya keras, juga untuk
  pembuatan bersamaan.
- **Role yang masih dipegang pengguna tidak dapat dihapus**, menurut
  `UserCounts` — termasuk yang dipegang pengguna nonaktif. `LockAssignments`
  mengambil, di transaksi penghapusannya, kunci pemberian akses, sehingga
  menghapus dan memberikan role tidak pernah berselang. Tanpanya, role yang
  diberikan tepat saat dihapus menjadi key tanpa role, yang tidak memegang
  izin apa pun.
- **Setiap perubahan dicatat di jejak audit**, di transaksi yang sama:
  `role.created`, `role.updated`, `role.deleted`, dengan isi role sebelum dan
  sesudahnya. Catatan bertahan setelah rolenya dihapus.

### Bentuk

`GET /roles`:

```json
{
  "roles": [
    { "key": "administrator", "name": "Administrator", "description": "", "audience": "internal", "builtin": true, "permissions": ["settings.users.manage", "…"], "version": 0, "updated_at": null },
    { "key": "5f0c2d1e-…", "name": "Kasir", "description": "", "audience": "internal", "builtin": false, "permissions": ["notes.read"], "version": 2, "updated_at": "2026-10-03T03:04:05Z" }
  ],
  "users": { "administrator": 1, "5f0c2d1e-…": 3 },
  "permissions": [
    { "name": "notes.read", "group": "Catatan", "label": "Melihat catatan", "audience": "internal", "sensitive": false }
  ],
  "custom": { "enabled": true, "count": 1, "max": 30 }
}
```

- `key` adalah yang disimpan produk di penggunanya: `Builtin.Key` untuk role
  bawaan, UUID untuk role buatan. Ia tidak berubah saat role diganti namanya.
- `custom.enabled` false berarti paketnya tidak menyertakan role buatan:
  layar menampilkan role yang ada tanpa tombol ubah dan tambah.
- Body `POST /roles` dan `PUT /roles/{key}`: `name` (wajib, maksimal 60
  karakter, unik per organization tanpa membedakan huruf besar-kecil),
  `description` (maksimal 200), `audience`, `permissions` (minimal satu), dan
  `version` (hanya untuk `PUT`). Field lain ditolak.
- `audience` kosong saat membuat berarti `internal`. Saat mengubah ia boleh
  kosong, dan bila diisi harus sama dengan audiens role itu.

## Pengguna

Siapa yang berhak masuk ke sebuah organization, dan dengan role apa. Pengguna
lahir dari pemberian akses yang disengaja, tidak pernah dari login pertama:
orang yang tidak dikenal ditolak, bukan dibuatkan akun.

Yang **tidak** ada di modul ini, dan tetap milik produk: sandi (tidak ada
kolomnya), login, sesi, dan pembuatan akun di penyedia identitas. Library ini
tidak pernah memanggil platform; yang dibutuhkan diminta lewat `Options`.

```go
people, err = users.New(pool, access, trail, hooks, users.Options{
	// Batas pengguna AKTIF, dari hak pakai paket.
	Seats: func(ctx context.Context, org uuid.UUID) (int64, error) {
		max, unlimited := license.Limit(ctx, entitlement.UsersMax)
		if unlimited {
			return users.Unlimited, nil
		}
		return max, nil
	},
	// Membuatkan atau menemukan akun login di penyedia identitas produk.
	Provision: func(ctx context.Context, email, name string) (users.Identity, error) {
		id, err := identities.Provision(ctx, email, name)
		return users.Identity{Subject: id.Subject, Email: id.Email, Name: id.DisplayName, TemporaryPassword: id.TemporaryPassword}, err
	},
	Available: func(context.Context) bool { return identities.Available() },
	// Sesi adalah tabel produk; dicabut di transaksi yang sama.
	RevokeSessions: func(ctx context.Context, tx pgx.Tx, org, user uuid.UUID) error {
		return sessions.RevokeAll(ctx, tx, org, user)
	},
})
```

Tiga jalan pemberian akses, satu aturan:

| jalan | fungsi | sesi |
|---|---|---|
| layar Pengguna & Akses | `Invite`, `Update` (endpoint di atas) | izin `settings.users.manage` |
| perintah operator | `Grant`, `Suspend` dengan `users.SourceOperator` | tanpa sesi |
| pemilik organization saat masuk pertama | `Grant` dengan `users.SourceOwner` | tanpa sesi |

Untuk login dan sesi produk, semuanya dengan `org` eksplisit:

```go
func (s *Service) BySubject(ctx context.Context, org uuid.UUID, subject string) (User, error)
func (s *Service) ByID(ctx context.Context, org, id uuid.UUID) (User, error)
func (s *Service) RecordLogin(ctx context.Context, org uuid.UUID, subject, email, name string) (User, error)
func (s *Service) All(ctx context.Context, org uuid.UUID) ([]User, error)
```

- Kunci orangnya `subject` — pengenalnya di penyedia identitas (klaim `sub`) —
  bukan email: email berubah, `sub` tidak.
- `RecordLogin` hanya berhasil untuk pengguna aktif: ia memperbarui waktu
  masuk terakhir, menyamakan email dan nama dengan penyedia identitas, dan
  mencatat `session.signed_in` di jejak audit. Orang yang tidak dikenal atau
  dinonaktifkan dijawab "tidak ditemukan", dan produk menolak loginnya.
- Produk memeriksa `status` pengguna pada setiap permintaan (`ByID`, atau
  `JOIN` ke `appkit_users` dari tabel sesinya): yang dinonaktifkan kehilangan
  akses saat itu juga, dan role baru berlaku pada permintaan berikutnya.
- Tabel sesi produk boleh merujuk `appkit_users (organization_id, id)`.

### Pagar

- **Batas pengguna ditegakkan di setiap pemberian akses**, di dalam kunci per
  organization: dua pemberian bersamaan tidak sama-sama lolos. Yang dihitung
  seluruh pengguna aktif. `users.Unlimited` berarti tanpa batas; **nol berarti
  tidak boleh ada pengguna aktif baru**, bukan tanpa batas. Batas yang penuh
  dijawab galat `KindQuotaExceeded`.
- **Tidak ada yang dapat menonaktifkan dirinya sendiri.**
- **Administrator aktif terakhir tidak dapat diturunkan atau dinonaktifkan**
  lewat layar. Jalan operator tidak dibatasi: itu jalan pemulihan.
- **Role yang diberikan harus ada**, dan dibaca sesudah kunci diambil, jadi
  role yang sedang dihapus tidak dapat diberikan.
- **Jenis orangnya tidak berubah.** Layar ini hanya memberikan role
  `internal`; role pengganti harus seaudiens dengan role sekarang. Orang luar
  butuh ikatan ke pihaknya (misalnya pelanggannya) yang hanya diketahui
  produk, jadi aksesnya diberikan alur produk lewat `Grant`.
- **Menonaktifkan mencabut sesi** di transaksi yang sama, lewat
  `RevokeSessions`.
- **Setiap perubahan akses dicatat di jejak audit**: `user.access_granted`,
  `user.role_changed`, `user.suspended`, `user.reactivated`, dengan jalannya
  (`screen`, `operator`, `owner`) di `details.source`.
- **Sandi sementara tidak pernah disimpan atau dicatat.** Ia hanya ada di
  jawaban `POST /users`, yang dikirim dengan `Cache-Control: no-store`. Jangan
  lewatkan endpoint itu ke modul idempotency.

### Bentuk

`GET /users`:

```json
{
  "data": [
    { "id": "0199a4c1-…", "subject": "abc123", "email": "ani@contoh.example", "name": "Ani Wijaya", "role": "administrator", "status": "active", "last_login_at": "2026-10-03T03:04:05Z", "created_at": "2026-10-01T03:04:05Z", "is_self": true }
  ],
  "invite": { "available": true },
  "seats": { "active": 1, "max": 5 },
  "roles": [ { "key": "administrator", "name": "Administrator", "…": "…" } ]
}
```

- `role` adalah key role; nama tampilnya dicari di `roles`, yang memuat
  seluruh role organization itu.
- `seats.max` `null` berarti tanpa batas. `invite.available` false disertai
  `invite.reason`.
- Body `POST /users`: `email`, `name`, `role`. Jawabannya `{user, account,
  temporary_password}`; `account` bernilai `created` atau `existing`, dan
  `temporary_password` hanya ada bila akunnya baru dibuat.
- Body `PATCH /users/{id}`: `role` dan/atau `status` (`active`, `suspended`);
  yang kosong tidak diubah.

## Jejak audit

Siapa melakukan apa, kapan, dan dari alamat mana, per organization. Catatan
**hanya dapat ditambah**: tidak ada jalur mengubah atau menghapusnya, dan
database menolak `UPDATE` atas tabelnya. Berapa lama catatan disimpan belum
ditetapkan, jadi tidak ada penghapusan otomatis.

| kelompok | isinya | siapa yang mencatat |
|---|---|---|
| `access` | akses diberikan atau dicabut, role pengguna diubah, role dibuat atau diubah izinnya | `users`, `roles` |
| `settings` | profil bisnis, website, pengaturan lain | `businessprofile`, `website`; produk untuk pengaturannya sendiri |
| `session` | masuk, keluar, sesi yang diputus | `users.RecordLogin` untuk masuk; produk untuk keluar dan sesi yang diputus |
| `activity` | tindakan penting milik produk: menyetujui dokumen, menghapus, mengubah harga | produk |

Yang **tidak** dicatat: pembacaan data, dan isi yang rahasia. Untuk sebuah
perubahan cukup apa yang diubah — modul library mencatat nama isian yang
berubah, bukan nilainya.

Modul library mencatat sendiri, di transaksi yang sama dengan perubahannya:

| tindakan | rincian (`details`) |
|---|---|
| `business_profile.updated` | `fields`: isian yang berubah |
| `business_profile.logo_changed`, `business_profile.logo_removed` | — |
| `website.updated` | `fields`: isian yang berubah |
| `website.image_changed`, `website.image_removed` | `slot` |
| `role.created`, `role.updated`, `role.deleted` | `before`, `after`: isi role |
| `numbering.scheme_updated` | `fields`: isian yang berubah |
| `user.access_granted`, `user.suspended` | `role`, `source` |
| `user.role_changed`, `user.reactivated` | `role_before`, `role_after`, `source` |
| `session.signed_in` | — |

Produk mencatat miliknya:

```go
// Di transaksi yang sama dengan perubahannya: catatan tersimpan hanya bila
// perubahannya tersimpan, dan sebaliknya.
err := trail.RecordTx(ctx, tx, audit.Entry{
	Category: audit.CategoryActivity,
	Action:   "document.approved",
	Target:   audit.Target{Type: "document", ID: doc.Number},
	Summary:  "SPK " + doc.Number + " disetujui.",
})

// Tanpa sesi — berhasil masuk, perintah operator, sesi yang diputus sistem
// (pelaku uuid.Nil):
err = trail.RecordFor(ctx, org, userID, audit.Entry{
	Category: audit.CategorySession, Action: "session.signed_in", Summary: "Masuk.",
})
```

- `RecordForTx` adalah `RecordFor` di dalam transaksi pemanggil.
- `Record` dan `RecordTx` membaca organization dan pelaku dari pengait;
  keduanya tidak memeriksa izin, karena yang dicatat adalah tindakan yang
  sudah diizinkan pemanggilnya.
- `Action` berbentuk huruf kecil bertitik, `<benda>.<kejadian>`. Nama yang
  sudah dipakai tidak diubah maknanya: catatan lama tetap menyebutnya.
- `Summary` adalah satu kalimat untuk layar riwayat, wajib. `Details` paling
  besar 16 KB sesudah dijadikan JSON.
- Alamat asal diminta lewat `audit.Options.ClientAddr`, karena hanya produk
  yang mengetahuinya di belakang proxy. Tanpa itu catatan tidak beralamat.
- Percobaan masuk yang gagal terjadi di halaman masuk platform dan tidak
  terlihat produk, jadi tidak ada di sini.

`GET /audit-events` (izin `settings.audit.view`):

```json
{
  "data": [
    {
      "id": "0199a4c1-…",
      "category": "access",
      "action": "role.updated",
      "actor_id": "7b1e…",
      "target": { "type": "role", "id": "5f0c2d1e-…" },
      "summary": "Role “Kasir” diubah.",
      "details": { "before": { "…": "…" }, "after": { "…": "…" } },
      "client_addr": "203.0.113.7",
      "created_at": "2026-10-03T03:04:05Z"
    }
  ],
  "next": "0199a4c1-…"
}
```

- `category` menyaring satu kelompok; `limit` bawaannya 50, paling banyak
  200. `next` dikirim balik sebagai `before` untuk halaman berikutnya; `null`
  di halaman terakhir.
- `actor_id` adalah id pengguna di dalam produk; `null` bila bukan tindakan
  seorang pengguna. Nama pelakunya dicari produk dari id itu.

## Idempotency

Mutasi yang diulang klien — karena timeout, koneksi putus, atau tombol ditekan
dua kali — tidak boleh menghasilkan dokumen ganda. Modul ini tidak punya
endpoint; modul produk yang membuat dokumen memakainya di dalam transaksinya:

```go
idem, err := idempotency.New(pool, idempotency.Options{}) // TTL bawaan 24 jam

// Di handler: key dari header, sidik jari dari body.
key, err := idempotency.Key(r)
fingerprint := idempotency.Fingerprint(body)

// Di service:
scope := idempotency.Scope{OrganizationID: org, Endpoint: "POST /v1/invoices", Key: key, Fingerprint: fingerprint}
if resp, err := idem.Replay(ctx, scope); err != nil || resp != nil {
	return resp, err // sudah pernah: putar ulang response-nya byte per byte
}
tx, err := pool.Begin(ctx)
defer func() { _ = tx.Rollback(ctx) }()
claimed, err := idem.Claim(ctx, tx, scope)
if !claimed {
	// Permintaan lain meng-commit key yang sama lebih dulu.
	_ = tx.Rollback(ctx)
	if resp, err := idem.Replay(ctx, scope); err != nil || resp != nil {
		return resp, err
	}
	return nil, appkit.IdempotencyConflict("Permintaan dengan kunci yang sama sedang diproses.")
}
// … membuat dokumen di tx, menyusun body response …
resp := idempotency.Response{Status: http.StatusCreated, Body: body}
if err := idem.Complete(ctx, tx, scope, resp); err != nil {
	return nil, err
}
return &resp, tx.Commit(ctx)
```

- Klaim, dokumen, dan response-nya di-commit bersama, jadi tidak ada keadaan
  "dokumen sudah ada tetapi key belum tercatat". Transaksi yang dibatalkan
  melepas klaimnya.
- Key berlaku per organization dan per `Endpoint`, selama `Options.TTL`. Key
  yang sama dengan isi berbeda dijawab galat jenis
  `appkit.KindIdempotencyConflict`; petakan di `Hooks.WriteError` produk.
- Header `Idempotency-Key` berisi 8–200 karakter huruf, angka, atau `. _ : -`.
- **Response yang membawa rahasia tidak boleh lewat modul ini** — misalnya
  jawaban `POST /users` yang membawa sandi sementara: response yang tersimpan
  ikut tersimpan di database selama key-nya hidup.
- Library ini tidak menjalankan pekerjaan latar. Produk memanggil
  `idem.DeleteExpired(ctx)` secara berkala untuk membuang baris kedaluwarsa.

## Penomoran dokumen

Nomor untuk dokumen apa pun milik produk: order, faktur, surat jalan. Jenis
dokumen didaftarkan produk di kode, jadi menambah jenis tidak menuntut
migrasi; bentuk nomornya milik organization, dan administratornya dapat
menggantinya dari layar pengaturan.

```go
numbers, err := numbering.New(pool, trail, hooks, numbering.Options{
	Types: []numbering.Type{
		{Key: "invoice", Label: "Faktur", Pattern: "INV/{YYYY}/{SEQ:05}", Reset: numbering.ResetYearly},
		{Key: "delivery", Label: "Surat Jalan", Pattern: "SJ/{YYYYMM}/{SEQ:04}", Reset: numbering.ResetMonthly},
	},
	// Zona waktu organization menurut produk: tahun dan bulan pada nomor
	// mengikuti waktu setempat, bukan UTC. Kosong: Asia/Jakarta untuk semua.
	Timezone: tenant.Timezone,
})

// Di dalam transaksi dokumennya:
number, err := numbers.Next(ctx, tx, org, "invoice", time.Now())
```

- **Tanpa nomor kembar dan tanpa nomor lompat**, juga saat dua dokumen
  disimpan bersamaan. `Next` wajib dipanggil di dalam transaksi dokumennya:
  dokumen yang batal disimpan mengembalikan nomornya, dan dokumen berikutnya
  memakainya.
- **Harganya:** dua pengguna yang membuat jenis dokumen yang sama di
  organization yang sama bergiliran sampai transaksi yang pertama selesai.
  Karena itu transaksi dokumen harus singkat, dan `Options.Timezone` — yang
  dipanggil selagi transaksinya terbuka — harus menjawab dari memori.
  Transaksi yang menomori beberapa jenis dokumen mengambilnya dalam urutan
  yang tetap.
- **Token pola:** `{YYYY}`, `{YY}`, `{YYYYMM}`, `{MM}`, `{SEQ}`, dan `{SEQ:NN}`
  untuk nomor berangka nol di depan. Pola wajib memuat `{SEQ}`, paling panjang
  60 karakter.
- **Kapan nomor kembali ke 1:** `never`, `yearly`, atau `monthly`. Pola dengan
  reset tahunan wajib memuat tahun, dan reset bulanan wajib memuat tahun dan
  bulan; tanpa itu nomor urut kembali ke 1 sedangkan nomor yang tercetak
  tidak berubah, dan dua dokumen mendapat nomor yang sama.
- **Mengubah pola tidak menyentuh nomor urut**: nomor yang sudah terbit tidak
  berubah diam-diam. Mengubah kebijakan reset membawa nomor urut yang sedang
  berjalan ke lingkup barunya, supaya nomor yang sama tidak terbit dua kali.
- Baris skema hanya ada bila organization mengganti bawaannya, jadi
  organization baru tidak perlu disiapkan. Perubahan skema dicatat di jejak
  audit sebagai `numbering.scheme_updated`.
- Pagar terakhir tetap di produk: beri indeks unik pada nomor dokumennya per
  organization. Dokumen bertanggal mundur ke periode yang dinomori dengan
  kebijakan lama masih dapat bertabrakan.

`GET /document-numbering`:

```json
{
  "data": [
    { "document_type": "invoice", "label": "Faktur", "pattern": "INV/{YYYY}/{SEQ:05}", "reset_policy": "yearly", "custom": false, "next_number": "INV/2026/00042", "version": 0, "updated_at": null }
  ],
  "tokens": [ { "token": "{YYYY}", "meaning": "tahun empat angka, mis. 2026" } ]
}
```

- `next_number` adalah contoh nomor berikutnya, dihitung tanpa
  mengalokasikannya. `custom` false berarti bawaan dari kode yang berlaku.
- Body `PUT /document-numbering/{type}`: `pattern`, `reset_policy`, `version`.

## Lampiran privat

Berkas yang hanya boleh dilihat orang yang berhak: lampiran surat perintah
kerja, pindaian kontrak, foto pemeriksaan mutu. Ini pasangan privat `media`:
berkas yang tampil tanpa sesi tempatnya di `media`, berkas yang butuh izin
tempatnya di sini.

Modul ini **tidak punya endpoint dan tidak punya jalur baca publik**. Siapa
yang boleh membaca atau mengunggah lampiran yang mana diputuskan modul produk
pemilik dokumennya: ia memeriksa izin atas dokumen itu, lalu memanggil modul
ini dengan dokumen yang sama sebagai pemilik.

```go
var files *attachments.Service

images, err := media.New(pool, hooks, media.Options{
	Quota: storageQuota,
	// Kuota penyimpanan satu, dipakai berdua: media menghitung lampiran...
	OtherUsage: func(ctx context.Context, org uuid.UUID) (int64, error) {
		u, err := files.Usage(ctx, org)
		return u.Bytes, err
	},
})
files, err = attachments.New(pool, attachments.Options{
	Store: store, // penyimpanan yang sama dengan media; kosong: database
	Quota: storageQuota,
	// ...dan lampiran menghitung media.
	OtherUsage: func(ctx context.Context, org uuid.UUID) (int64, error) {
		u, err := images.Usage(ctx, org)
		return u.Bytes, err
	},
})

// Di handler produk, SETELAH izin atas dokumen orderID diperiksa:
owner := attachments.Owner{Type: "work_order", ID: orderID}
f, err := files.Save(ctx, org, owner, header.Filename, userID, body)

f, content, err := files.Open(ctx, org, owner, fileID)
defer content.Close()
attachments.Serve(w, r, f, content)
```

- **Berkas hanya terbaca dan terhapus lewat dokumen pemiliknya.** `Get`,
  `Open`, dan `Delete` meminta `Owner`; berkas dokumen lain — walau satu
  organization — dijawab "tidak ditemukan". Izin atas satu dokumen tidak
  pernah membuka lampiran dokumen lain, dan id berkas dari URL tidak perlu
  diperiksa lagi.
- Setiap baca dan hapus menyaring organization, tanpa pengecualian.
- **Jenis berkas dikenali dari isinya**, bukan dari nama atau header klien.
  Bawaannya PDF, PNG, JPEG, dan WebP (`Options.Types`). SVG dan HTML tidak
  pernah diterima, apa pun pengaturannya. Berkas Office (`.docx`, `.xlsx`)
  terbaca sebagai `application/zip`.
- **Selalu disajikan sebagai unduhan** (`Serve`): `Content-Disposition:
  attachment`, `Cache-Control: private, no-store`, tidak pernah ditampilkan
  di dalam halaman.
- Batas ukuran satu berkas bawaannya 10 MB (`Options.MaxBytes`).
- Isinya disimpan lewat `media.Store` di bawah key berawalan `private/`, jadi
  tidak bertabrakan dengan berkas media walau penyimpanannya sama.
- `files.DeleteOwner(ctx, org, owner)` menghapus seluruh lampiran satu
  dokumen, untuk dipanggil saat dokumennya dihapus.
- Modul ini tidak mencatat ke jejak audit: yang mencatat unggahan dan
  penghapusan lampiran adalah modul pemilik dokumennya, seperti logo dicatat
  `businessprofile`.

## Media

- Setiap berkas dapat dibaca **siapa pun** yang mengetahui URL-nya. Jangan
  simpan berkas yang butuh izin di sini.
- Jenisnya dikenali dari isi berkas. SVG ditolak.
- Batas ukuran satu berkas bawaannya 2 MB (`media.Options.MaxBytes`). Itu
  pengaman teknis, bukan batas yang dijual.

### Penyimpanan

Isi berkas disimpan lewat antarmuka `media.Store`; data tentang berkasnya
selalu di tabel `appkit_media`. Mengganti `Store` tidak mengubah skema, API,
maupun URL berkas.

| penyimpanan | isi berkas | cocok untuk |
|---|---|---|
| `media.DBStore` (bawaan) | tabel `appkit_media_blobs`; ikut masuk backup database | self-host, dan pemasangan tanpa object storage |
| `s3store.Store` | bucket yang berbicara API S3: Cloudflare R2, AWS S3, dan sejenisnya | cloud |

```go
store, err := s3store.New(s3store.Options{
	Endpoint:        endpoint,  // R2: https://<akun>.r2.cloudflarestorage.com
	Region:          region,    // R2: "auto"
	Bucket:          bucket,    // harus sudah ada
	AccessKeyID:     accessKey,
	SecretAccessKey: secretKey,
})
if err != nil {
	return err
}
// Salah konfigurasi ketahuan saat start, bukan di unggahan pertama.
if err := store.Check(ctx); err != nil {
	return err
}
files, err := media.New(pool, hooks, media.Options{Store: store})
```

- Library tidak membaca environment; nilai `Options` diisi produk dari
  konfigurasinya sendiri.
- **Berpindah dari database ke S3 tidak butuh pemindahan data.** Berkas baru
  masuk ke bucket; berkas yang isinya sudah di database tetap terbaca dan
  tetap dapat dihapus. Arah sebaliknya tidak: berkas yang isinya di bucket
  tidak terbaca setelah `Store` dikembalikan ke database.
- Berkas tetap disajikan produk di `/media/{id}`; bucket-nya tidak perlu
  dibuka untuk umum.
- `s3store` tidak memuat kode khusus satu penyedia. Untuk layanan yang
  memakai alamat `https://host/bucket/key`, isi `PathStyle: true`.

### Kuota

Yang dibatasi adalah **total** penyimpanan satu organization. Produk
menyerahkan batasnya — biasanya dari hak pakai paket — lewat `Options.Quota`:

```go
files, err := media.New(pool, hooks, media.Options{
	Quota: func(ctx context.Context, org uuid.UUID) (int64, error) {
		gb, unlimited := license.Limit(ctx, entitlement.StorageGB)
		if unlimited {
			return media.Unlimited, nil
		}
		return gb << 30, nil // byte
	},
})
```

- `media.Unlimited` berarti tanpa batas. **Nol berarti tidak boleh menyimpan
  sama sekali**, bukan tanpa batas — hak pakai yang tidak dibawa paket
  dijawab nol. Tanpa `Quota`, semua organization tanpa batas.
- Unggahan yang melewati batas ditolak dengan galat jenis
  `appkit.KindQuotaExceeded`; petakan di `Hooks.WriteError` produk.
- Mengganti gambar (logo, gambar website) tidak menghitung berkas yang
  digantikannya, jadi kuota yang penuh tidak mengunci organization dari
  mengganti gambarnya. Modul lain memakai `files.SaveReplacing` untuk itu.
- `files.Usage(ctx, org)` menjawab pemakaian sekarang (byte dan jumlah
  berkas), untuk ditampilkan produk.
- `media.Options.OtherUsage` menambahkan pemakaian di tempat lain — biasanya
  lampiran privat — ke hitungan kuota yang sama; lihat Lampiran privat.
- Batasnya lunak: dua unggahan bersamaan dapat sama-sama lolos dan
  melewatinya sebesar satu berkas.

## Mengembangkan library ini

```
make test   # seluruh test terhadap PostgreSQL dan S3 lokal (docker compose)
make lint   # go vet dan gofmt
```

Layanan S3 lokalnya S3Proxy: ia memeriksa tanda tangan permintaan seperti
penyedia sungguhan. Library ini belum diuji terhadap akun R2 nyata.

Aturan kontribusi ada di `AGENTS.md`.

## Perubahan

- **v0.4.0**
  - Baru: modul `audit` — jejak audit per organization, hanya dapat ditambah.
  - Baru: modul `roles` — role bawaan di kode, role buatan per organization,
    dan `PermissionsOf`/`Can` untuk `Hooks.Authorize` produk.
  - Baru: modul `users` — pengguna per organization, pemberian akses, batas
    pengguna, dan pengaman administrator terakhir.
  - Baru: modul `idempotency`, dan galat jenis baru
    `appkit.KindIdempotencyConflict` — tambahkan pemetaannya di
    `Hooks.WriteError` produk.
  - Baru: modul `numbering` — nomor dokumen tanpa kembar dan tanpa lompat.
  - Baru: modul `attachments` — berkas privat yang terbaca hanya lewat
    dokumen pemiliknya.
  - Baru: `media.Options.OtherUsage` dan `media.WithDBFallback`. Tanpa
    `OtherUsage` perilaku kuota media tidak berubah.
  - Baru: pengait `Hooks.User`, id pengguna di dalam produk. `Hooks.Validate`
    tidak memintanya; `audit.New` yang meminta.
  - **Memutus:** modul `website` kini hanya menyimpan identitas halaman
    depan. Bagian "tentang" (`about`, slot gambar `about`) dan daftar layanan
    (`services`, `icons`) dibuang dari pengaturan dan dari tampilan publik;
    field lamanya di `PUT /website` kini ditolak. Migrasi `00011` membuang
    kolom gambar "tentang" beserta berkasnya.
  - **Memutus:** `businessprofile.New` dan `website.New` kini menerima service
    jejak audit (`businessprofile.New(pool, files, trail, hooks)`,
    `website.New(pool, profiles, files, trail, hooks, opts)`), dan setiap
    perubahan profil bisnis serta website dicatat. Permintaan yang mengubah
    keduanya kini butuh `Hooks.User`: tanpa pelaku, perubahannya ditolak.
  - Migrasi baru: `00005` (`appkit_audit_events`), `00006` (`appkit_roles`),
    `00007` (`appkit_users`), `00008` (`appkit_idempotency_keys`), `00009`
    (`appkit_number_schemes`, `appkit_number_counters`), `00010`
    (`appkit_attachments`), dan `00011` (lihat di atas). Tabelnya terpasang di
    setiap produk, dipakai atau tidak.
- **v0.3.0**
  - Baru: `media/s3store` — isi berkas di object storage yang berbicara API
    S3 (Cloudflare R2, AWS S3). Berkas lama di database tetap terbaca.
  - Baru: kuota penyimpanan per organization (`media.Options.Quota`,
    `media.Unlimited`), `Service.Usage`, dan `Service.SaveReplacing`.
  - Baru: galat jenis `appkit.KindQuotaExceeded` — tambahkan pemetaannya di
    `Hooks.WriteError` produk. Tanpa `Options.Quota` galat ini tidak pernah
    muncul.
  - Tidak ada migrasi baru dan tidak ada yang memutus.
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
