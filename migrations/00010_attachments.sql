-- Lampiran: berkas PRIVAT milik sebuah organization — lampiran surat perintah
-- kerja, pindaian kontrak, foto pemeriksaan mutu. Kebalikan appkit_media, yang
-- berkasnya dapat dibaca siapa pun yang mengetahui URL-nya.
--
-- Tabel ini hanya memuat data TENTANG berkas. Isinya disimpan lewat
-- media.Store: di appkit_media_blobs untuk penyimpanan bawaan (key berawalan
-- "private/"), atau di object storage. Karena itu tidak ada tabel isi baru.
--
-- owner_type dan owner_id menyebut dokumen yang dilampiri, mis. "work_order"
-- dan id-nya. Tanpa foreign key: dokumen itu baris di tabel produk, dan tabel
-- library tidak merujuk tabel produk. Yang menghapus lampiran saat dokumennya
-- dihapus adalah modul pemilik dokumen itu.
--
-- content_type tidak dibatasi CHECK berisi daftar: jenis yang diterima diatur
-- produk (attachments.Options.Types), dan menambahnya tidak boleh menuntut
-- migrasi. Yang dijaga di sini hanya yang tidak pernah boleh, apa pun
-- pengaturannya: SVG dan HTML, dokumen yang dapat membawa skrip.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_attachments (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    owner_type      text        NOT NULL CHECK (owner_type ~ '^[a-z][a-z0-9_]{0,59}$'),
    owner_id        text        NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 200),
    -- Nama asli berkas, hanya untuk nama unduhan. Tidak pernah menjadi key
    -- penyimpanan.
    filename        text        NOT NULL CHECK (length(filename) BETWEEN 1 AND 200),
    content_type    text        NOT NULL CHECK (
        length(content_type) <= 100
        AND content_type ~ '^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$'
        AND content_type NOT IN ('image/svg+xml', 'text/html', 'application/xhtml+xml')
    ),
    size_bytes      bigint      NOT NULL CHECK (size_bytes > 0),
    sha256          text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    -- Id pengguna di dalam produk. NULL: bukan tindakan seorang pengguna,
    -- misalnya berkas yang dibuat sistem.
    uploaded_by     uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Sasaran foreign key KOMPOSIT dari tabel produk: rujukan lintas
    -- organization ditolak database, bukan hanya kode.
    UNIQUE (organization_id, id)
);

-- Daftar lampiran satu dokumen, terlama dulu.
CREATE INDEX appkit_attachments_organization_owner_created_at_idx
    ON appkit_attachments (organization_id, owner_type, owner_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_attachments;
-- Isi yang tersimpan di database ikut dibuang: tanpa barisnya, tidak ada lagi
-- yang dapat membukanya. Isi di object storage di luar jangkauan migrasi.
DELETE FROM appkit_media_blobs WHERE key LIKE 'private/%';
-- +goose StatementEnd
