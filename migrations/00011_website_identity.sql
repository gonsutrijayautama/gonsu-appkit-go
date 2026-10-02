-- Pengaturan website kini hanya IDENTITAS halaman depan: mode, tagline,
-- ringkasan, kontak, kanal, dan pratinjau tautan. Isi halaman — bagian
-- "tentang" dan daftar layanan — bukan lagi urusan tabel ini; tempatnya di
-- penyusun halaman.
--
-- Karena itu kolom gambar bagian "tentang" dibuang, bersama berkasnya:
-- berkas yang tidak dirujuk siapa pun tetap terhitung ke kuota penyimpanan.
-- Isi berkas yang tersimpan di database ikut terhapus; yang di object storage
-- di luar jangkauan migrasi dan tertinggal di sana.
--
-- Dokumen settings tidak disentuh: kunci about_text dan services yang
-- tertinggal diabaikan kode, dan hilang pada simpan berikutnya.

-- +goose Up
-- +goose StatementBegin
CREATE TEMPORARY TABLE appkit_dropped_about_images ON COMMIT DROP AS
    SELECT organization_id, about_media_id AS id
    FROM appkit_websites
    WHERE about_media_id IS NOT NULL;

-- Membuang kolom ikut membuang foreign key-nya, sehingga berkasnya dapat
-- dihapus.
ALTER TABLE appkit_websites DROP COLUMN about_media_id;

DELETE FROM appkit_media_blobs b
    USING appkit_dropped_about_images d
    WHERE b.key = d.organization_id::text || '/' || d.id::text;

DELETE FROM appkit_media m
    USING appkit_dropped_about_images d
    WHERE m.organization_id = d.organization_id AND m.id = d.id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Kolomnya kembali, gambarnya tidak.
ALTER TABLE appkit_websites
    ADD COLUMN about_media_id uuid,
    ADD FOREIGN KEY (organization_id, about_media_id) REFERENCES appkit_media (organization_id, id);
-- +goose StatementEnd
