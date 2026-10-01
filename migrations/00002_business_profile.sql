-- Profil bisnis: identitas organization di dalam produk. Satu baris per
-- organization.
--
-- Makna kolomnya sama dengan profil penagihan platform GONSU One
-- (business_type, legal_name, tax_id, alamat, kode pos, email, telepon),
-- supaya menyalin dari sana kelak tidak butuh penerjemahan. Dua perbedaan
-- yang disengaja:
--
--   - wilayah disimpan sebagai KODE Kemendagri (region_code), bukan nama.
--     Nama provinsi sampai desa diturunkan saat dibaca; nama kabupaten/kota
--     hasilnya sama persis dengan billing_city platform, karena datanya dari
--     library yang sama;
--   - isian kosong disimpan sebagai string kosong, bukan NULL: API-nya tidak
--     membedakan "belum diisi" dari "dikosongkan".
--
-- Tanpa kolom negara dan zona waktu: keduanya dapat ditambahkan belakangan
-- tanpa mengubah kolom yang ada.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_business_profiles (
    organization_id uuid        PRIMARY KEY,
    -- Kosong hanya sebelum profil pertama kali disimpan (logo boleh diunggah
    -- lebih dulu). Menyimpan profil mewajibkannya.
    display_name    text        NOT NULL DEFAULT '' CHECK (length(display_name) <= 120),
    industry        text        NOT NULL DEFAULT '' CHECK (length(industry) <= 120),
    email           text        NOT NULL DEFAULT '' CHECK (length(email) <= 320),
    phone           text        NOT NULL DEFAULT '' CHECK (length(phone) <= 32),
    business_type   text        NOT NULL DEFAULT '' CHECK (business_type IN ('', 'individual', 'company')),
    legal_name      text        NOT NULL DEFAULT '' CHECK (length(legal_name) <= 200),
    -- NPWP 16 digit tanpa pemisah, sama dengan platform.
    tax_id          text        NOT NULL DEFAULT '' CHECK (tax_id ~ '^([0-9]{16})?$'),
    address         text        NOT NULL DEFAULT '' CHECK (length(address) <= 500),
    -- Minimal kabupaten/kota ("32.73"), boleh sampai desa ("32.73.07.1001").
    region_code     text        NOT NULL DEFAULT ''
        CHECK (region_code ~ '^([0-9]{2}\.[0-9]{2}(\.[0-9]{2}(\.[0-9]{4})?)?)?$'),
    postcode        text        NOT NULL DEFAULT '' CHECK (postcode ~ '^([0-9]{5})?$'),
    logo_media_id   uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, logo_media_id) REFERENCES appkit_media (organization_id, id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_business_profiles;
-- +goose StatementEnd
