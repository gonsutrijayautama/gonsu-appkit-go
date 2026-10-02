-- Penomoran dokumen: bentuk nomor tiap jenis dokumen sebuah organization, dan
-- nomor urut terakhirnya.
--
-- Dua tabel:
--
--   appkit_number_schemes   pola dan kebijakan reset yang DIUBAH administrator
--                           sebuah organization. Barisnya hanya ada bila
--                           bawaannya diganti: tanpa baris, yang berlaku adalah
--                           bawaan yang didaftarkan kode produk. Karena itu
--                           tidak ada langkah mengisi baris untuk organization
--                           baru.
--   appkit_number_counters  nomor urut terakhir per lingkup. Lingkup (scope)
--                           mengikuti kebijakan reset: '-' untuk yang tidak
--                           pernah kembali ke 1, '2026' untuk tahunan, dan
--                           '2026-09' untuk bulanan.
--
-- document_type tidak dibatasi CHECK berisi daftar: jenis dokumen didaftarkan
-- kode produk, dan menambahnya tidak boleh menuntut migrasi. Baris milik jenis
-- yang sudah tidak terdaftar diabaikan kode.
--
-- Counter tidak punya foreign key ke skema: jenis dokumen yang memakai bawaan
-- tidak punya baris skema, tetapi tetap punya nomor urut.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_number_schemes (
    organization_id uuid        NOT NULL,
    document_type   text        NOT NULL CHECK (length(document_type) BETWEEN 1 AND 40),
    pattern         text        NOT NULL CHECK (length(pattern) BETWEEN 1 AND 60),
    reset_policy    text        NOT NULL CHECK (reset_policy IN ('never', 'yearly', 'monthly')),
    version         integer     NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, document_type)
);

CREATE TABLE appkit_number_counters (
    organization_id uuid   NOT NULL,
    document_type   text   NOT NULL CHECK (length(document_type) BETWEEN 1 AND 40),
    scope           text   NOT NULL CHECK (length(scope) BETWEEN 1 AND 20),
    -- Nomor urut yang terakhir dipakai. Barisnya baru ada setelah nomor pertama
    -- lingkup itu dipakai, jadi tidak pernah nol.
    last_value      bigint NOT NULL CHECK (last_value >= 1),
    PRIMARY KEY (organization_id, document_type, scope)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_number_counters;
DROP TABLE appkit_number_schemes;
-- +goose StatementEnd
