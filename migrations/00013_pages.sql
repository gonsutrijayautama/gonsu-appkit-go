-- Halaman: halaman publik sebuah organization yang disusun dengan penyusun
-- halaman (beranda, layanan, tentang, …).
--
--   appkit_pages            satu baris per halaman: alamat, judul, menu, SEO,
--                           DRAF yang sedang disusun, dan isi TERBIT yang
--                           dilihat pengunjung. Pengunjung tidak pernah
--                           melihat draf.
--   appkit_page_revisions   riwayat isi yang pernah diterbitkan, untuk
--                           dikembalikan ke draf. Hanya beberapa terakhir per
--                           halaman yang disimpan.
--   appkit_page_media       gambar yang diunggah untuk sebuah halaman, supaya
--                           gambar yang tidak dirujuk lagi dapat dihapus dan
--                           tidak terus menghabiskan kuota.
--   appkit_page_imports     organization yang sudah mengimpor isi website
--                           lamanya, supaya tawaran impornya hilang.
--
-- Isi halaman (draft, published, document) adalah data penyusun halaman di
-- frontend produk. Jenis bloknya didaftarkan produk di kode dan diperiksa
-- server saat disimpan, jadi tidak ada CHECK berisi daftar blok.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_pages (
    id                uuid        PRIMARY KEY,
    organization_id   uuid        NOT NULL,
    -- "/" untuk beranda; selain itu satu tingkat: "/layanan".
    path              text        NOT NULL CHECK (path = '/' OR (length(path) <= 61 AND path ~ '^/[a-z0-9]+(-[a-z0-9]+)*$')),
    title             text        NOT NULL CHECK (length(title) BETWEEN 1 AND 80),
    nav_visible       boolean     NOT NULL DEFAULT false,
    nav_position      integer     NOT NULL DEFAULT 0,
    seo_title         text        NOT NULL DEFAULT '' CHECK (length(seo_title) <= 70),
    seo_description   text        NOT NULL DEFAULT '' CHECK (length(seo_description) <= 160),
    seo_media_id      uuid,
    draft             jsonb       NOT NULL CHECK (jsonb_typeof(draft) = 'object'),
    -- Isi yang dilihat pengunjung; NULL bila belum atau tidak lagi terbit.
    published         jsonb       CHECK (published IS NULL OR jsonb_typeof(published) = 'object'),
    -- Nomor revisi yang sedang tampil; NULL bila tidak terbit.
    published_number  integer     CHECK ((published IS NULL) = (published_number IS NULL)),
    published_at      timestamptz,
    version           integer     NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, path),
    UNIQUE (organization_id, id),
    FOREIGN KEY (organization_id, seo_media_id) REFERENCES appkit_media (organization_id, id)
);

CREATE TABLE appkit_page_revisions (
    id                uuid        PRIMARY KEY,
    organization_id   uuid        NOT NULL,
    page_id           uuid        NOT NULL,
    number            integer     NOT NULL CHECK (number >= 1),
    title             text        NOT NULL,
    document          jsonb       NOT NULL CHECK (jsonb_typeof(document) = 'object'),
    published_at      timestamptz NOT NULL DEFAULT now(),
    -- Pengguna di dalam produk, dan namanya saat itu.
    published_by      uuid,
    published_by_name text        NOT NULL DEFAULT '' CHECK (length(published_by_name) <= 200),
    UNIQUE (organization_id, page_id, number),
    FOREIGN KEY (organization_id, page_id) REFERENCES appkit_pages (organization_id, id) ON DELETE CASCADE
);

CREATE TABLE appkit_page_media (
    organization_id   uuid        NOT NULL,
    page_id           uuid        NOT NULL,
    media_id          uuid        NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, media_id),
    FOREIGN KEY (organization_id, media_id) REFERENCES appkit_media (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX appkit_page_media_page_idx ON appkit_page_media (organization_id, page_id);

CREATE TABLE appkit_page_imports (
    organization_id   uuid        PRIMARY KEY,
    imported_at       timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_page_imports;
DROP TABLE appkit_page_media;
DROP TABLE appkit_page_revisions;
DROP TABLE appkit_pages;
-- +goose StatementEnd
