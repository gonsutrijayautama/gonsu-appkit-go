-- Pengguna: siapa yang BERHAK masuk ke sebuah organization, dan dengan role
-- apa.
--
-- Kuncinya external_subject — pengenal orang itu di penyedia identitas produk
-- (klaim `sub`) — bukan email: email berubah, `sub` tidak.
--
-- TIDAK ADA kolom sandi, dan tidak boleh ditambahkan: produk tidak menyimpan
-- sandi siapa pun. Sesi juga tidak di sini; itu tabel milik produk, yang
-- boleh merujuk (organization_id, id) tabel ini.
--
-- Riwayat akses — diberi, role diubah, dinonaktifkan, diaktifkan kembali —
-- ada di jejak audit (appkit_audit_events).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_users (
    id               uuid        PRIMARY KEY,
    organization_id  uuid        NOT NULL,
    external_subject text        NOT NULL CHECK (length(external_subject) BETWEEN 1 AND 255),
    -- Kosong bila belum diketahui. Mengikuti penyedia identitas pada setiap
    -- login.
    email            text        NOT NULL DEFAULT '' CHECK (length(email) <= 320),
    name             text        NOT NULL DEFAULT '' CHECK (length(name) <= 200),
    -- Key role (package roles): key role bawaan, atau UUID role buatan. Tanpa
    -- foreign key: role bawaan ada di kode dan tidak punya baris.
    role_key         text        NOT NULL CHECK (length(role_key) BETWEEN 1 AND 40),
    status           text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    last_login_at    timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, external_subject),
    -- Sasaran foreign key KOMPOSIT dari tabel produk (mis. sesi): rujukan
    -- lintas organization ditolak database, bukan hanya kode.
    UNIQUE (organization_id, id)
);

-- Menghitung pemegang tiap role, dan administrator aktif yang tersisa.
CREATE INDEX appkit_users_organization_role_idx ON appkit_users (organization_id, role_key);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_users;
-- +goose StatementEnd
