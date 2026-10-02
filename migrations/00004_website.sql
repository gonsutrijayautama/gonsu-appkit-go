-- Pengaturan website: bagaimana halaman depan sebuah organization tampil.
-- Satu baris per organization.
--
-- Isinya satu dokumen JSON (settings), bukan kolom per isian: isi halaman
-- depan akan terus bertambah — bagian baru, kanal baru — dan menambah isian
-- di dokumen tidak butuh migrasi. Dokumen membawa nomor `schema`-nya sendiri.
--
-- Gambar tetap berupa kolom: foreign key KOMPOSIT ke appkit_media membuat
-- database, bukan hanya kode, menolak rujukan ke berkas organization lain.
--
-- version menjaga simpan-bersamaan, seperti profil bisnis. Mengganti gambar
-- tidak menaikkannya.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_websites (
    organization_id uuid        PRIMARY KEY,
    settings        jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(settings) = 'object'),
    about_media_id  uuid,
    seo_media_id    uuid,
    version         integer     NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, about_media_id) REFERENCES appkit_media (organization_id, id),
    FOREIGN KEY (organization_id, seo_media_id) REFERENCES appkit_media (organization_id, id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_websites;
-- +goose StatementEnd
