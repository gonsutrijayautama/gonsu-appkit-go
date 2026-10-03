package pages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
)

// Batas isian halaman, sama dengan constraint kolomnya.
const (
	maxTitle          = 80
	maxPath           = 61
	maxSEOTitle       = 70
	maxSEODescription = 160
)

var rePath = regexp.MustCompile(`^/[a-z0-9]+(-[a-z0-9]+)*$`)

// emptyDocument adalah isi halaman baru: belum ada blok.
var emptyDocument = json.RawMessage(`{"content":[],"root":{"props":{}}}`)

// Nama tindakan di jejak audit, kelompok audit.CategorySettings. Target-nya
// {Type: TargetType, ID: Page.ID}. Menyimpan draf tidak dicatat: draf tidak
// terlihat pengunjung, dan terbitannya yang dicatat.
const (
	ActionCreated         = "page.created"
	ActionSettingsUpdated = "page.settings_updated"
	ActionPublished       = "page.published"
	ActionUnpublished     = "page.unpublished"
	ActionDeleted         = "page.deleted"
	ActionLegacyImported  = "page.legacy_imported"

	TargetType = "page"
)

func (s *Service) record(ctx context.Context, tx pgx.Tx, action string, p Page, summary string, details map[string]any) error {
	return s.trail.RecordTx(ctx, tx, audit.Entry{
		Category: audit.CategorySettings, Action: action,
		Target:  audit.Target{Type: TargetType, ID: p.ID.String()},
		Summary: summary, Details: details,
	})
}

var (
	errChanged = appkit.Conflict("Halaman ini sudah diubah orang lain. Muat ulang halaman, lalu ulangi perubahan Anda.")
	errEmpty   = appkit.Validation("Halaman tanpa blok tidak dapat diterbitkan.",
		appkit.FieldError{Field: "draft", Message: "Tambahkan minimal satu blok sebelum menerbitkan."})
)

// checkPath memeriksa alamat halaman baru atau pengganti.
func (s *Service) checkPath(path string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	switch {
	case path == HomePath:
		return ""
	case len(path) > maxPath || !rePath.MatchString(path):
		return "Alamat diawali \"/\", berisi huruf kecil, angka, dan tanda hubung, satu tingkat, maksimal 60 karakter."
	case s.reserved["/"+first]:
		return "Alamat ini dipakai aplikasi. Pilih alamat lain."
	}
	return ""
}

func violates(err error, code string) bool {
	e, ok := errors.AsType[*pgconn.PgError](err)
	return ok && e.Code == code
}

var errPathTaken = appkit.Validation("Isian belum sesuai.", appkit.FieldError{Field: "path", Message: "Alamat ini sudah dipakai halaman lain."})

// CreateInput adalah body POST /pages.
type CreateInput struct {
	Title string `json:"title"`
	// Path: HomePath untuk beranda, atau satu tingkat seperti "/layanan".
	Path string `json:"path"`
}

// Create membuat halaman kosong. Galatnya berjenis appkit.KindQuotaExceeded
// (appkit.LimitPages) bila batas halaman paketnya sudah penuh, atau paketnya
// tidak menyertakan penyusun halaman.
func (s *Service) Create(ctx context.Context, in CreateInput) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	limit, err := s.limit(ctx, org)
	if err != nil {
		return Page{}, err
	}
	if limit == 0 {
		return Page{}, errDisabled
	}
	in.Title, in.Path = strings.TrimSpace(in.Title), strings.TrimSpace(in.Path)
	var errs []appkit.FieldError
	if in.Title == "" || utf8.RuneCountInString(in.Title) > maxTitle {
		errs = append(errs, appkit.FieldError{Field: "title", Message: fmt.Sprintf("Judul wajib diisi, maksimal %d karakter.", maxTitle)})
	}
	if problem := s.checkPath(in.Path); problem != "" {
		errs = append(errs, appkit.FieldError{Field: "path", Message: problem})
	}
	if len(errs) > 0 {
		return Page{}, appkit.Validation("Isian belum lengkap.", errs...)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Dua pembuatan bersamaan bergiliran, supaya batas paket tidak terlewati.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "appkit_pages:"+org.String()); err != nil {
		return Page{}, fmt.Errorf("pages: mengunci pembuatan halaman: %w", err)
	}
	if limit > 0 {
		var count int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM appkit_pages WHERE organization_id = $1`, org).Scan(&count); err != nil {
			return Page{}, fmt.Errorf("pages: menghitung halaman: %w", err)
		}
		if count >= limit {
			return Page{}, appkit.QuotaExceeded(appkit.LimitPages, fmt.Sprintf(
				"Jumlah halaman sudah mencapai batas paket (%d). Hapus halaman yang tidak dipakai, atau naikkan paket.", limit))
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Page{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO appkit_pages (id, organization_id, path, title, draft) VALUES ($1, $2, $3, $4, $5)`,
		id, org, in.Path, in.Title, []byte(emptyDocument)); err != nil {
		if violates(err, "23505") {
			return Page{}, errPathTaken
		}
		return Page{}, fmt.Errorf("pages: menyimpan halaman: %w", err)
	}
	p, err := s.get(ctx, tx, org, id)
	if err != nil {
		return Page{}, err
	}
	if err := s.record(ctx, tx, ActionCreated, p, fmt.Sprintf("Halaman “%s” (%s) dibuat.", p.Title, p.Path),
		map[string]any{"path": p.Path}); err != nil {
		return Page{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Page{}, err
	}
	return p, nil
}

// Input adalah body PUT /pages/{id} dan POST /pages/{id}/publish. Seluruh
// isian dikirim setiap kali.
type Input struct {
	Title      string     `json:"title"`
	Path       string     `json:"path"`
	Navigation Navigation `json:"navigation"`
	SEO        struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"seo"`
	// Draft adalah data penyusun halaman.
	Draft json.RawMessage `json:"draft"`
	// Version adalah Page.Version yang dibaca sebelum mengubah.
	Version int `json:"version"`
}

// prepared adalah Input yang sudah diperiksa.
type prepared struct {
	Input
	refs map[uuid.UUID]bool
}

// prepare memeriksa in untuk halaman cur.
func (s *Service) prepare(ctx context.Context, org uuid.UUID, cur Page, in Input) (prepared, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Path = strings.TrimSpace(in.Path)
	in.SEO.Title = strings.TrimSpace(in.SEO.Title)
	in.SEO.Description = strings.TrimSpace(in.SEO.Description)
	var errs []appkit.FieldError
	fail := func(field, message string) { errs = append(errs, appkit.FieldError{Field: field, Message: message}) }

	if in.Version < 0 {
		fail("version", "Versi tidak sah.")
	}
	if in.Title == "" || utf8.RuneCountInString(in.Title) > maxTitle {
		fail("title", fmt.Sprintf("Judul wajib diisi, maksimal %d karakter.", maxTitle))
	}
	switch {
	case cur.Path == HomePath && in.Path != HomePath:
		fail("path", "Alamat beranda tidak dapat diganti.")
	case cur.Path != HomePath && in.Path == HomePath:
		fail("path", "Beranda dibuat sebagai halaman tersendiri.")
	case in.Path != cur.Path:
		if problem := s.checkPath(in.Path); problem != "" {
			fail("path", problem)
		}
	}
	if utf8.RuneCountInString(in.SEO.Title) > maxSEOTitle {
		fail("seo.title", fmt.Sprintf("Judul pratinjau maksimal %d karakter.", maxSEOTitle))
	}
	if utf8.RuneCountInString(in.SEO.Description) > maxSEODescription {
		fail("seo.description", fmt.Sprintf("Deskripsi pratinjau maksimal %d karakter.", maxSEODescription))
	}
	clean, refs, problems := s.checkDocument(in.Draft)
	if len(problems) > 0 {
		fail("draft", "Isi halaman belum sesuai: "+strings.Join(problems, "; ")+".")
	}
	if len(errs) > 0 {
		return prepared{}, appkit.Validation("Isian belum lengkap.", errs...)
	}
	// Gambar hanya boleh berkas media organization ini sendiri.
	if missing, err := s.missingMedia(ctx, org, refs); err != nil {
		return prepared{}, err
	} else if len(missing) > 0 {
		return prepared{}, appkit.Validation("Isian belum lengkap.", appkit.FieldError{
			Field: "draft", Message: "Ada gambar yang tidak ditemukan. Unggah ulang gambarnya lewat halaman ini."})
	}
	in.Draft = clean
	return prepared{Input: in, refs: refs}, nil
}

func (s *Service) missingMedia(ctx context.Context, org uuid.UUID, refs map[uuid.UUID]bool) ([]uuid.UUID, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM appkit_media WHERE organization_id = $1 AND id = ANY($2)`, org, ids)
	if err != nil {
		return nil, fmt.Errorf("pages: memeriksa gambar: %w", err)
	}
	defer rows.Close()
	found := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		found[id] = true
	}
	var missing []uuid.UUID
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return missing, rows.Err()
}

// settingsChanged menyebut isian pengaturan yang berubah, untuk jejak audit.
func settingsChanged(cur Page, in Input) []string {
	var out []string
	for _, f := range []struct {
		name    string
		differs bool
	}{
		{"title", cur.Title != in.Title},
		{"path", cur.Path != in.Path},
		{"navigation", cur.Navigation != in.Navigation},
		{"seo.title", cur.SEO.Title != in.SEO.Title},
		{"seo.description", cur.SEO.Description != in.SEO.Description},
	} {
		if f.differs {
			out = append(out, f.name)
		}
	}
	return out
}

// Update menyimpan seluruh isian halaman id: pengaturan dan draf. Pengaturan
// — judul, alamat, menu, SEO — langsung berlaku; isi baru terlihat
// pengunjung sesudah diterbitkan.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (Page, error) {
	return s.save(ctx, id, in, false)
}

// Publish menyimpan seluruh isian seperti Update, lalu menerbitkan drafnya —
// dalam satu transaksi, sehingga yang terbit tepat yang terakhir dilihat
// penyusunnya. Halaman tanpa blok tidak dapat diterbitkan.
func (s *Service) Publish(ctx context.Context, id uuid.UUID, in Input) (Page, error) {
	return s.save(ctx, id, in, true)
}

func (s *Service) save(ctx context.Context, id uuid.UUID, in Input, publish bool) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	if err := s.requireEnabled(ctx, org); err != nil {
		return Page{}, err
	}
	actor, err := s.hooks.User(ctx)
	if err != nil {
		return Page{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Baris dikunci dulu: version yang dibandingkan adalah version yang akan
	// diganti.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM appkit_pages WHERE organization_id = $1 AND id = $2 FOR UPDATE`, org, id); err != nil {
		return Page{}, err
	}
	cur, err := s.get(ctx, tx, org, id)
	if err != nil {
		return Page{}, err
	}
	p, err := s.prepare(ctx, org, cur, in)
	if err != nil {
		return Page{}, err
	}
	if p.Version != cur.Version {
		return Page{}, errChanged
	}
	if publish && !hasBlocks(p.Draft) {
		return Page{}, errEmpty
	}

	if _, err := tx.Exec(ctx, `
		UPDATE appkit_pages
		SET title = $3, path = $4, nav_visible = $5, nav_position = $6, seo_title = $7, seo_description = $8,
		    draft = $9, version = version + 1, updated_at = now()
		WHERE organization_id = $1 AND id = $2`,
		org, id, p.Title, p.Path, p.Navigation.Visible, p.Navigation.Position, p.SEO.Title, p.SEO.Description,
		[]byte(p.Draft)); err != nil {
		if violates(err, "23505") {
			return Page{}, errPathTaken
		}
		return Page{}, fmt.Errorf("pages: menyimpan halaman: %w", err)
	}
	if fields := settingsChanged(cur, p.Input); len(fields) > 0 {
		if err := s.record(ctx, tx, ActionSettingsUpdated, cur, fmt.Sprintf("Pengaturan halaman “%s” diubah.", p.Title),
			map[string]any{"fields": fields}); err != nil {
			return Page{}, err
		}
	}
	if publish {
		if err := s.publish(ctx, tx, org, cur, p, actor); err != nil {
			return Page{}, err
		}
	}
	out, err := s.get(ctx, tx, org, id)
	if err != nil {
		return Page{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Page{}, err
	}
	s.collect(ctx, org)
	return out, nil
}

func hasBlocks(doc json.RawMessage) bool {
	var d struct {
		Content []json.RawMessage `json:"content"`
	}
	return json.Unmarshal(doc, &d) == nil && len(d.Content) > 0
}

// publish menjadikan draf p isi terbit halaman cur, mencatatnya di riwayat,
// dan membuang terbitan lama di luar Options.MaxRevisions.
func (s *Service) publish(ctx context.Context, tx pgx.Tx, org uuid.UUID, cur Page, p prepared, actor uuid.UUID) error {
	var number int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(max(number), 0) + 1 FROM appkit_page_revisions WHERE organization_id = $1 AND page_id = $2`,
		org, cur.ID).Scan(&number); err != nil {
		return fmt.Errorf("pages: menomori terbitan: %w", err)
	}
	rev, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO appkit_page_revisions (id, organization_id, page_id, number, title, document, published_by, published_by_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		rev, org, cur.ID, number, p.Title, []byte(p.Draft), actor, s.trail.ActorName(ctx, org, actor)); err != nil {
		return fmt.Errorf("pages: mencatat terbitan: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE appkit_pages SET published = draft, published_number = $3, published_at = now()
		WHERE organization_id = $1 AND id = $2`, org, cur.ID, number); err != nil {
		return fmt.Errorf("pages: menerbitkan halaman: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM appkit_page_revisions
		WHERE organization_id = $1 AND page_id = $2 AND number <= $3`,
		org, cur.ID, number-s.opts.MaxRevisions); err != nil {
		return fmt.Errorf("pages: membuang terbitan lama: %w", err)
	}
	return s.record(ctx, tx, ActionPublished, cur, fmt.Sprintf("Halaman “%s” diterbitkan (terbitan %d).", p.Title, number),
		map[string]any{"number": number})
}

// Unpublish menarik halaman id dari pengunjung. Drafnya tetap. Boleh juga
// saat paketnya tidak lagi menyertakan penyusun halaman: menarik halaman
// mengurangi yang tampil, seperti menghapus.
func (s *Service) Unpublish(ctx context.Context, id uuid.UUID) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE appkit_pages SET published = NULL, published_number = NULL, published_at = NULL, updated_at = now()
		WHERE organization_id = $1 AND id = $2 AND published IS NOT NULL`, org, id)
	if err != nil {
		return Page{}, fmt.Errorf("pages: menarik halaman: %w", err)
	}
	p, err := s.get(ctx, tx, org, id)
	if err != nil {
		return Page{}, err
	}
	// Menarik halaman yang tidak terbit tidak mengubah apa pun.
	if tag.RowsAffected() == 1 {
		if err := s.record(ctx, tx, ActionUnpublished, p, fmt.Sprintf("Halaman “%s” tidak lagi diterbitkan.", p.Title), nil); err != nil {
			return Page{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Page{}, err
	}
	return p, nil
}

// Delete menghapus halaman id beserta riwayat dan gambarnya. Boleh juga saat
// paketnya tidak lagi menyertakan penyusun halaman.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	org, err := s.begin(ctx)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := s.get(ctx, tx, org, id)
	if err != nil {
		return err
	}
	var seo *uuid.UUID
	if err := tx.QueryRow(ctx, `
		DELETE FROM appkit_pages WHERE organization_id = $1 AND id = $2 RETURNING seo_media_id`, org, id).Scan(&seo); err != nil {
		return fmt.Errorf("pages: menghapus halaman: %w", err)
	}
	// Gambar halaman ini dilepas dari halamannya; yang tidak dirujuk halaman
	// lain dihapus collect sesudah commit.
	if _, err := tx.Exec(ctx, `
		UPDATE appkit_page_media SET created_at = '-infinity' WHERE organization_id = $1 AND page_id = $2`, org, id); err != nil {
		return fmt.Errorf("pages: melepas gambar halaman: %w", err)
	}
	if err := s.record(ctx, tx, ActionDeleted, p, fmt.Sprintf("Halaman “%s” (%s) dihapus.", p.Title, p.Path),
		map[string]any{"path": p.Path}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if seo != nil {
		s.dropMedia(ctx, org, *seo)
	}
	s.collect(ctx, org)
	return nil
}

// Revision adalah satu terbitan di riwayat.
type Revision struct {
	ID     uuid.UUID `json:"id"`
	Number int       `json:"number"`
	// Title adalah judul halaman saat diterbitkan.
	Title       string     `json:"title"`
	PublishedAt time.Time  `json:"published_at"`
	PublishedBy *uuid.UUID `json:"published_by"`
	// PublishedByName adalah nama penerbitnya saat itu; kosong bila tidak
	// diketahui.
	PublishedByName string `json:"published_by_name"`
	// Live menandai terbitan yang sedang tampil.
	Live bool `json:"live"`
}

// Revisions mengembalikan riwayat terbitan halaman id, terbaru dulu.
func (s *Service) Revisions(ctx context.Context, id uuid.UUID) ([]Revision, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.get(ctx, s.pool, org, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.number, r.title, r.published_at, r.published_by, r.published_by_name,
		       r.number IS NOT DISTINCT FROM p.published_number
		FROM appkit_page_revisions r
		JOIN appkit_pages p ON p.organization_id = r.organization_id AND p.id = r.page_id
		WHERE r.organization_id = $1 AND r.page_id = $2
		ORDER BY r.number DESC`, org, id)
	if err != nil {
		return nil, fmt.Errorf("pages: membaca riwayat: %w", err)
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.ID, &r.Number, &r.Title, &r.PublishedAt, &r.PublishedBy, &r.PublishedByName, &r.Live); err != nil {
			return nil, fmt.Errorf("pages: membaca riwayat: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RestoreInput adalah body POST /pages/{id}/revisions/{revision}/restore.
type RestoreInput struct {
	Version int `json:"version"`
}

// Restore menjadikan isi terbitan revision draf halaman id. Yang tampil ke
// pengunjung tidak berubah sampai drafnya diterbitkan.
func (s *Service) Restore(ctx context.Context, id, revision uuid.UUID, in RestoreInput) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	if err := s.requireEnabled(ctx, org); err != nil {
		return Page{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE appkit_pages p SET draft = r.document, version = p.version + 1, updated_at = now()
		FROM appkit_page_revisions r
		WHERE p.organization_id = $1 AND p.id = $2 AND p.version = $4
		  AND r.organization_id = p.organization_id AND r.page_id = p.id AND r.id = $3`,
		org, id, revision, in.Version)
	if err != nil {
		return Page{}, fmt.Errorf("pages: mengembalikan terbitan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Halaman atau terbitannya tidak ada, atau version-nya sudah berubah.
		cur, err := s.get(ctx, tx, org, id)
		if err != nil {
			return Page{}, err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM appkit_page_revisions
			WHERE organization_id = $1 AND page_id = $2 AND id = $3)`, org, id, revision).Scan(&exists); err != nil {
			return Page{}, err
		}
		if !exists {
			return Page{}, appkit.NotFound("Terbitan tidak ditemukan.")
		}
		if cur.Version != in.Version {
			return Page{}, errChanged
		}
		return Page{}, errors.New("pages: terbitan tidak dapat dikembalikan")
	}
	out, err := s.get(ctx, tx, org, id)
	if err != nil {
		return Page{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Page{}, err
	}
	s.collect(ctx, org)
	return out, nil
}

// legacyAvailable: isi website lama ada, dapat dipetakan produk, dan belum
// diimpor.
func (s *Service) legacyAvailable(ctx context.Context, org uuid.UUID) (bool, error) {
	if s.opts.ImportLegacy == nil {
		return false, nil
	}
	var imported bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM appkit_page_imports WHERE organization_id = $1)`, org).Scan(&imported); err != nil {
		return false, fmt.Errorf("pages: memeriksa impor: %w", err)
	}
	if imported {
		return false, nil
	}
	legacy, err := s.sites.Legacy(ctx, org)
	if err != nil {
		return false, err
	}
	return legacy.AboutText != "" || len(legacy.Services) > 0 || legacy.AboutImage != nil, nil
}

// ImportLegacy mengisi DRAF beranda dari isi halaman website versi lama,
// lewat Options.ImportLegacy milik produk. Beranda dibuat bila belum ada.
// Sesudahnya tawaran impor hilang. Isi lama di website tetap utuh.
func (s *Service) ImportLegacy(ctx context.Context) (Page, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Page{}, err
	}
	if err := s.requireEnabled(ctx, org); err != nil {
		return Page{}, err
	}
	available, err := s.legacyAvailable(ctx, org)
	if err != nil {
		return Page{}, err
	}
	if !available {
		return Page{}, appkit.NotFound("Tidak ada isi website lama yang dapat diimpor.")
	}
	legacy, err := s.sites.Legacy(ctx, org)
	if err != nil {
		return Page{}, err
	}
	doc, err := s.opts.ImportLegacy(ctx, legacy)
	if err != nil {
		return Page{}, err
	}
	clean, refs, problems := s.checkDocument(doc)
	if len(problems) > 0 {
		return Page{}, fmt.Errorf("pages: Options.ImportLegacy menghasilkan isi yang tidak sah: %s", strings.Join(problems, "; "))
	}
	if missing, err := s.missingMedia(ctx, org, refs); err != nil {
		return Page{}, err
	} else if len(missing) > 0 {
		return Page{}, fmt.Errorf("pages: Options.ImportLegacy merujuk gambar yang tidak ada: %v", missing)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "appkit_pages:"+org.String()); err != nil {
		return Page{}, err
	}
	// Membuat beranda baru menghitung terhadap batas paket.
	var home bool
	var count int64
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM appkit_pages WHERE organization_id = $1 AND path = '/'),
		       (SELECT count(*) FROM appkit_pages WHERE organization_id = $1)`, org).Scan(&home, &count); err != nil {
		return Page{}, err
	}
	if limit, err := s.limit(ctx, org); err != nil {
		return Page{}, err
	} else if !home && limit > 0 && count >= limit {
		return Page{}, appkit.QuotaExceeded(appkit.LimitPages, fmt.Sprintf(
			"Jumlah halaman sudah mencapai batas paket (%d). Hapus halaman yang tidak dipakai, atau naikkan paket.", limit))
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Page{}, err
	}
	// Beranda yang sudah ada mendapat draf hasil impor; yang belum ada dibuat.
	if err := tx.QueryRow(ctx, `
		INSERT INTO appkit_pages (id, organization_id, path, title, draft) VALUES ($1, $2, '/', 'Beranda', $3)
		ON CONFLICT (organization_id, path) DO UPDATE
		SET draft = EXCLUDED.draft, version = appkit_pages.version + 1, updated_at = now()
		RETURNING id`, id, org, []byte(clean)).Scan(&id); err != nil {
		return Page{}, fmt.Errorf("pages: menyimpan beranda: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO appkit_page_imports (organization_id) VALUES ($1) ON CONFLICT DO NOTHING`, org); err != nil {
		return Page{}, fmt.Errorf("pages: mencatat impor: %w", err)
	}
	p, err := s.get(ctx, tx, org, id)
	if err != nil {
		return Page{}, err
	}
	if err := s.record(ctx, tx, ActionLegacyImported, p, "Isi website lama diimpor ke draf beranda.", nil); err != nil {
		return Page{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Page{}, err
	}
	return p, nil
}
