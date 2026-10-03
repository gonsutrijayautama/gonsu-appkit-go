package pages

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
)

// UploadImage menyimpan satu gambar untuk blok halaman id dan mengembalikan
// berkasnya; isian gambar blok merujuk File.URL. Gambar ikut kuota
// penyimpanan, dan dihapus sendiri bila tidak lagi dirujuk halaman mana pun
// — draf, terbitan, maupun riwayat — sejak sehari sesudah diunggah.
func (s *Service) UploadImage(ctx context.Context, id uuid.UUID, r io.Reader) (media.File, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return media.File{}, err
	}
	if err := s.requireEnabled(ctx, org); err != nil {
		return media.File{}, err
	}
	if _, err := s.get(ctx, s.pool, org, id); err != nil {
		return media.File{}, err
	}
	f, err := s.files.Save(ctx, org, r)
	if err != nil {
		return media.File{}, err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO appkit_page_media (organization_id, page_id, media_id) VALUES ($1, $2, $3)`, org, id, f.ID); err != nil {
		// Berkasnya belum dirujuk siapa pun; jangan ditinggal menjadi sampah.
		s.dropMedia(context.WithoutCancel(ctx), org, f.ID)
		return media.File{}, fmt.Errorf("pages: mencatat gambar: %w", err)
	}
	return f, nil
}

// SetSEOImage mengganti gambar pratinjau tautan halaman id. Gambar lama
// dihapus. Langsung berlaku, seperti pengaturan halaman lainnya.
func (s *Service) SetSEOImage(ctx context.Context, id uuid.UUID, r io.Reader) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	if err := s.requireEnabled(ctx, org); err != nil {
		return Page{}, err
	}
	var current *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT seo_media_id FROM appkit_pages WHERE organization_id = $1 AND id = $2`, org, id).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Page{}, errNotFound
		}
		return Page{}, fmt.Errorf("pages: membaca gambar pratinjau: %w", err)
	}
	f, err := s.files.SaveReplacing(ctx, org, r, current)
	if err != nil {
		return Page{}, err
	}
	p, err := s.swapSEOImage(ctx, org, id, &f.ID)
	if err != nil {
		s.dropMedia(context.WithoutCancel(ctx), org, f.ID)
		return Page{}, err
	}
	return p, nil
}

// RemoveSEOImage menghapus gambar pratinjau tautan halaman id. Halaman tanpa
// gambar bukan galat.
func (s *Service) RemoveSEOImage(ctx context.Context, id uuid.UUID) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	return s.swapSEOImage(ctx, org, id, nil)
}

func (s *Service) swapSEOImage(ctx context.Context, org, id uuid.UUID, image *uuid.UUID) (Page, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var old *uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT seo_media_id FROM appkit_pages WHERE organization_id = $1 AND id = $2 FOR UPDATE`, org, id).Scan(&old); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Page{}, errNotFound
		}
		return Page{}, fmt.Errorf("pages: mengunci halaman: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE appkit_pages SET seo_media_id = $3, updated_at = now() WHERE organization_id = $1 AND id = $2`,
		org, id, image); err != nil {
		return Page{}, fmt.Errorf("pages: memasang gambar pratinjau: %w", err)
	}
	p, err := s.get(ctx, tx, org, id)
	if err != nil {
		return Page{}, err
	}
	if image != nil || old != nil {
		if err := s.record(ctx, tx, ActionSettingsUpdated, p, fmt.Sprintf("Pengaturan halaman “%s” diubah.", p.Title),
			map[string]any{"fields": []string{"seo.image"}}); err != nil {
			return Page{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Page{}, err
	}
	if old != nil {
		s.dropMedia(context.WithoutCancel(ctx), org, *old)
	}
	return p, nil
}

// dropMedia menghapus satu berkas. Gagal menghapus tidak membatalkan apa pun:
// yang tertinggal hanya berkas yang tidak dirujuk.
func (s *Service) dropMedia(ctx context.Context, org, id uuid.UUID) {
	if err := s.files.Delete(ctx, org, id); err != nil {
		s.opts.Logger.WarnContext(ctx, "pages: berkas tidak terhapus", "organization_id", org.String(), "media_id", id.String(), "error", err.Error())
	}
}

// collect menghapus gambar halaman org yang tidak lagi dirujuk draf,
// terbitan, maupun riwayat halaman mana pun. Gambar yang baru diunggah diberi
// waktu sehari: ia boleh belum masuk draf yang tersimpan.
//
// Dipanggil sesudah isi halaman berubah. Kegagalannya hanya dicatat: gambar
// yang tertinggal akan terhapus pada perubahan berikutnya.
func (s *Service) collect(ctx context.Context, org uuid.UUID) {
	ctx = context.WithoutCancel(ctx)
	rows, err := s.pool.Query(ctx, `
		SELECT m.media_id FROM appkit_page_media m
		WHERE m.organization_id = $1 AND m.created_at < now() - interval '1 day'
		  AND NOT EXISTS (
		        SELECT 1 FROM appkit_pages p
		        WHERE p.organization_id = m.organization_id
		          AND (strpos(p.draft::text, m.media_id::text) > 0
		               OR strpos(COALESCE(p.published::text, ''), m.media_id::text) > 0
		               OR p.seo_media_id = m.media_id))
		  AND NOT EXISTS (
		        SELECT 1 FROM appkit_page_revisions r
		        WHERE r.organization_id = m.organization_id AND strpos(r.document::text, m.media_id::text) > 0)`, org)
	if err != nil {
		s.opts.Logger.WarnContext(ctx, "pages: gambar tidak terpakai tidak terbaca", "organization_id", org.String(), "error", err.Error())
		return
	}
	var stale []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := s.pool.Exec(ctx, `DELETE FROM appkit_page_media WHERE organization_id = $1 AND media_id = $2`, org, id); err != nil {
			continue
		}
		s.dropMedia(ctx, org, id)
	}
}
