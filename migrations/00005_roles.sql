-- Role buatan: kumpulan izin bernama yang disusun administrator sebuah
-- organization.
--
-- Yang TIDAK ada di sini, dengan sengaja:
--
--   - izin. Izin didefinisikan kode produk; kolom permissions hanya menyimpan
--     namanya, dan nama yang sudah tidak dikenal kode diabaikan saat dibaca;
--   - role bawaan. Ia ada di kode dan tidak punya baris;
--   - siapa memegang role apa. Itu kolom di tabel pengguna milik produk, dan
--     tabel library tidak merujuk tabel produk.
--
-- appkit_role_events adalah jejak perubahannya. Ia tidak punya foreign key ke
-- appkit_roles: jejak sebuah role tetap ada setelah role itu dihapus.

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

CREATE TABLE appkit_role_events (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    role_id         uuid        NOT NULL,
    action          text        NOT NULL CHECK (action IN ('created', 'updated', 'deleted')),
    -- Id pengguna di dalam produk (Hooks.User).
    actor_id        uuid        NOT NULL,
    -- Isi role sebelum dan sesudah perubahan: nama, keterangan, audiens, izin.
    old_state       jsonb       CHECK (old_state IS NULL OR jsonb_typeof(old_state) = 'object'),
    new_state       jsonb       CHECK (new_state IS NULL OR jsonb_typeof(new_state) = 'object'),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((action = 'created') = (old_state IS NULL)),
    CHECK ((action = 'deleted') = (new_state IS NULL))
);

CREATE INDEX appkit_role_events_organization_created_at_idx
    ON appkit_role_events (organization_id, created_at DESC, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_role_events;
DROP TABLE appkit_roles;
-- +goose StatementEnd
