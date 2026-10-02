-- Role buatan: kumpulan izin bernama yang disusun administrator sebuah
-- organization.
--
-- Yang TIDAK ada di sini, dengan sengaja:
--
--   - izin. Izin didefinisikan kode produk; kolom permissions hanya menyimpan
--     namanya, dan nama yang sudah tidak dikenal kode diabaikan saat dibaca;
--   - role bawaan. Ia ada di kode dan tidak punya baris;
--   - siapa memegang role apa. Itu kolom di tabel pengguna milik produk, dan
--     tabel library tidak merujuk tabel produk;
--   - riwayat perubahannya. Itu jejak audit (appkit_audit_events).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_roles (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    name            text        NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    description     text        NOT NULL DEFAULT '' CHECK (length(description) <= 200),
    -- Tidak berubah setelah dibuat: role staf tidak pernah menjadi role orang
    -- luar, atau sebaliknya, selagi ada yang memegangnya.
    audience        text        NOT NULL CHECK (audience IN ('internal', 'external')),
    permissions     text[]      NOT NULL DEFAULT '{}' CHECK (array_position(permissions, NULL) IS NULL),
    version         integer     NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- Sasaran foreign key KOMPOSIT dari tabel produk: rujukan lintas
    -- organization ditolak database, bukan hanya kode.
    UNIQUE (organization_id, id)
);

-- Dua role satu organization tidak boleh hanya berbeda huruf besar-kecil.
CREATE UNIQUE INDEX appkit_roles_organization_name_key ON appkit_roles (organization_id, lower(name));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_roles;
-- +goose StatementEnd
