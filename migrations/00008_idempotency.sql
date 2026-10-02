-- Idempotency key untuk mutasi: scope per organization dan endpoint, sidik
-- jari request, dan response yang diputar ulang byte per byte.
--
-- Barisnya ditulis di transaksi yang sama dengan dokumen yang dibuat mutasi
-- itu, sehingga key dan dokumennya tersimpan bersama atau tidak sama sekali.
--
-- Baris yang kedaluwarsa tidak dihapus sendiri: produk memanggil
-- idempotency.DeleteExpired secara berkala. Sampai dihapus, baris kedaluwarsa
-- diperlakukan seperti tidak ada, dan key-nya dapat diklaim ulang.
--
-- Tanpa foreign key ke tabel mana pun: tabel library tidak merujuk tabel
-- produk.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_idempotency_keys (
    organization_id     uuid        NOT NULL,
    -- Nama tetap operasinya, mis. "POST /v1/invoices". Key yang sama di dua
    -- endpoint adalah dua key.
    endpoint            text        NOT NULL CHECK (length(endpoint) BETWEEN 1 AND 200),
    idempotency_key     text        NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    -- Key sama dengan isi berbeda adalah kesalahan client, bukan pengulangan.
    request_fingerprint text        NOT NULL CHECK (length(request_fingerprint) BETWEEN 1 AND 200),
    -- NULL: key sudah diklaim, response-nya belum disimpan.
    response_status     integer     CHECK (response_status BETWEEN 100 AND 599),
    -- bytea, bukan jsonb: jsonb menormalisasi JSON, sehingga response yang
    -- diputar ulang tidak lagi identik dengan yang pertama.
    response_body       bytea,
    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,
    PRIMARY KEY (organization_id, endpoint, idempotency_key),
    -- Body tanpa status bukan response.
    CHECK (response_status IS NOT NULL OR response_body IS NULL)
);

-- Untuk penghapusan baris kedaluwarsa.
CREATE INDEX appkit_idempotency_keys_expires_at_idx ON appkit_idempotency_keys (expires_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_idempotency_keys;
-- +goose StatementEnd
