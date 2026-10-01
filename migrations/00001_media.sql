-- Media: berkas publik milik sebuah organization (logo, gambar halaman depan).
--
-- Dua tabel, sengaja dipisah:
--
--   appkit_media        data TENTANG berkas. Selalu di database.
--   appkit_media_blobs  ISI berkas, untuk penyimpanan bawaan (media.DBStore).
--                       Produk yang memakai object storage tidak mengisinya.
--
-- Karena itu appkit_media_blobs tidak punya foreign key ke appkit_media:
-- penyimpanan adalah antarmuka, dan urutan tulisnya diatur service.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_media (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    content_type    text        NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg', 'image/webp')),
    size_bytes      bigint      NOT NULL CHECK (size_bytes > 0),
    sha256          text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    -- NULL bila ukuran gambar tidak dapat dibaca tanpa decoder tambahan (WebP).
    width           integer     CHECK (width IS NULL OR width > 0),
    height          integer     CHECK (height IS NULL OR height > 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Sasaran foreign key KOMPOSIT dari tabel lain: rujukan lintas
    -- organization ditolak database, bukan hanya kode.
    UNIQUE (organization_id, id)
);

CREATE TABLE appkit_media_blobs (
    key     text  PRIMARY KEY,
    content bytea NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_media_blobs;
DROP TABLE appkit_media;
-- +goose StatementEnd
