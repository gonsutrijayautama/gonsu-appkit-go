-- Nama pelaku dicatat bersama id-nya, saat tindakan dilakukan.
--
-- Tanpa ini layar riwayat aktivitas harus mencari nama dari id pelaku, dan
-- pemegang izin membaca riwayat belum tentu boleh membaca daftar pengguna.
-- Nama yang dicatat adalah nama SAAT ITU: mengganti nama sesudahnya tidak
-- mengubah catatan lama, sama seperti isi catatan lainnya.
--
-- Catatan lama mendapat nama kosong.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE appkit_audit_events
    ADD COLUMN actor_name text NOT NULL DEFAULT '' CHECK (length(actor_name) <= 200);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE appkit_audit_events DROP COLUMN actor_name;
-- +goose StatementEnd
