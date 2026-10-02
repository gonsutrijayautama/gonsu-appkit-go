-- Jejak audit: siapa melakukan apa, kapan, dan dari alamat mana, per
-- organization.
--
-- Catatan HANYA DAPAT DITAMBAH. Modul tidak punya jalur mengubah atau
-- menghapusnya, dan trigger di bawah membuat database sendiri menolak UPDATE:
-- catatan yang dapat disunting bukan jejak. DELETE tidak ditolak di sini —
-- berapa lama catatan disimpan belum ditetapkan, dan menghapus seluruh data
-- sebuah organization tetap hak pemasangnya.
--
-- category dan action tidak dibatasi CHECK berisi daftar: jenis tindakan
-- bertambah bersama modul dan produk, dan menambahnya tidak boleh menuntut
-- migrasi. Daftarnya dijaga kode.
--
-- Tanpa foreign key ke tabel mana pun: catatan tentang sesuatu tetap ada
-- setelah yang dicatat itu dihapus.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE appkit_audit_events (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL,
    category        text        NOT NULL CHECK (length(category) BETWEEN 1 AND 40),
    action          text        NOT NULL CHECK (length(action) BETWEEN 1 AND 100),
    -- Id pengguna di dalam produk (Hooks.User). NULL: bukan tindakan seorang
    -- pengguna, misalnya sesi yang diputus sistem.
    actor_id        uuid,
    -- Yang dikenai tindakan; kosong bila tidak ada.
    target_type     text        NOT NULL DEFAULT '' CHECK (length(target_type) <= 60),
    target_id       text        NOT NULL DEFAULT '' CHECK (length(target_id) <= 200),
    -- Satu kalimat untuk layar riwayat.
    summary         text        NOT NULL CHECK (length(summary) BETWEEN 1 AND 300),
    -- Rincian: apa yang diubah. Tidak pernah memuat isi yang rahasia.
    details         jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(details) = 'object'),
    client_addr     inet,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX appkit_audit_events_organization_created_at_idx
    ON appkit_audit_events (organization_id, created_at DESC, id DESC);

CREATE FUNCTION appkit_audit_events_reject_update() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'appkit_audit_events hanya dapat ditambah';
END;
$$;

CREATE TRIGGER appkit_audit_events_append_only
    BEFORE UPDATE ON appkit_audit_events
    FOR EACH ROW EXECUTE FUNCTION appkit_audit_events_reject_update();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE appkit_audit_events;
DROP FUNCTION appkit_audit_events_reject_update();
-- +goose StatementEnd
