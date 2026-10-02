# AGENTS.md — gonsu-appkit-go

Library modul standar produk GONSU: `businessprofile`, `website`, `media`
(dengan `media/s3store`), dan `regions`.
Dipasang produk lewat `go get`; project hasil `gonsu new` memakainya.
Repository ini PUBLIK supaya `go get` berjalan tanpa kredensial, tetapi
lisensinya proprietary (lihat `LICENSE`).

## Aturan

- **Bahasa.** Identifier, nama berkas, tabel, kolom, dan path URL dalam bahasa
  Inggris. Komentar, pesan galat yang tampil ke pengguna, dan dokumen dalam
  bahasa Indonesia.
- **Bukan SDK platform.** Library ini tidak pernah memanggil GONSU One dan
  tidak meng-import `gonsu-one-sdk-go`. Kontrak dengan platform (lisensi,
  login) tempatnya di SDK.
- **Tidak meng-import kode produk.** Yang dibutuhkan dari produk diminta lewat
  pengait di `appkit.Hooks`: organization, izin, dan penulisan galat. Galat
  dari pengait diteruskan apa adanya.
- **organization hanya dari `Hooks.Organization`**, tidak pernah dari body,
  query, atau environment. Setiap query menyaring `organization_id`; data
  milik organization lain dijawab "tidak ditemukan". Satu-satunya pengecualian
  adalah jalur baca publik `media.Open`, dan pengecualian baru harus
  dijelaskan di komentar fungsinya.
- **Tabel berawalan `appkit_`**, dan migrasinya di `migrations/` dengan tabel
  versi `appkit_schema_migrations`. Migrasi yang sudah dirilis tidak diubah;
  perubahan skema adalah berkas migrasi baru. Tabel library tidak merujuk
  tabel produk.
- **Tanpa nilai milik satu pemasangan dan tanpa environment berawalan
  `GONSU_`.** Awalan itu kontrak platform. Konfigurasi masuk lewat `Options`
  yang diisi produk.
- **Kontrak.** Selama v0.x bentuknya masih boleh berubah, dan setiap perubahan
  yang memutus dicatat di `README.md` bagian Perubahan. Sejak v1.0.0 kontrak
  hanya boleh BERTAMBAH: path `/v1`, field JSON, kolom, dan tanda tangan
  fungsi publik tidak dihapus atau diubah maknanya.
- **Test wajib**, terhadap PostgreSQL dan layanan S3 nyata (`make test`).
  Setiap modul menguji jalur tanpa izin dan jalur lintas organization, bukan
  hanya jalur berhasil.
- **Tanpa kode khusus satu penyedia penyimpanan.** `media/s3store` berbicara
  API S3 umum; yang membedakan R2, AWS, dan lainnya hanya `Options`.
- **CI hanya memakai runner GitHub**, tidak pernah self-hosted.
- **Dokumen ikut kode**, di PR yang sama: `README.md` untuk cara memasang dan
  daftar endpoint.
