-- Sengaja tidak melakukan apa-apa.
--
-- Versi awal migrasi ini — yang tidak pernah dirilis — membuang foto bagian
-- "tentang" dari pengaturan website beserta berkasnya. Itu isi milik
-- pelanggan, dan tidak boleh hilang sebelum penyusun halaman dapat
-- mengimpornya; kini package website membiarkannya utuh (website.Legacy).
--
-- Nomornya tetap dipakai supaya database yang sempat menerapkan versi awal
-- itu dan database baru berada di nomor yang sama.

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
