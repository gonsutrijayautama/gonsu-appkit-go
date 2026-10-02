-- Penjaga simpan-bersamaan untuk profil bisnis: setiap simpan membawa version
-- yang dibacanya, dan ditolak bila profil sudah berubah sejak itu. Tanpanya dua
-- administrator yang membuka formulir bersamaan saling menimpa diam-diam.
--
-- Baris yang sudah ada mulai dari 0, sama dengan profil yang belum pernah
-- disimpan. Mengganti logo tidak menaikkan version: logo bukan bagian formulir.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE appkit_business_profiles
    ADD COLUMN version integer NOT NULL DEFAULT 0 CHECK (version >= 0);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE appkit_business_profiles DROP COLUMN version;
-- +goose StatementEnd
