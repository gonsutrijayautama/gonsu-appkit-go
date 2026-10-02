package numbering

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
)

// Scheme adalah skema penomoran satu jenis dokumen di API.
type Scheme struct {
	// DocumentType adalah Type.Key.
	DocumentType string `json:"document_type"`
	Label        string `json:"label"`
	Pattern      string `json:"pattern"`
	// ResetPolicy: ResetNever, ResetYearly, atau ResetMonthly.
	ResetPolicy string `json:"reset_policy"`
	// Custom false: organization ini memakai bawaan dari kode.
	Custom bool `json:"custom"`
	// NextNumber adalah CONTOH nomor berikutnya, dihitung dari nomor urut
	// lingkup yang sedang berjalan tanpa mengalokasikannya: melihat contoh
	// tidak boleh memakai nomor.
	//
	// Ia contoh, bukan janji: dokumen yang disimpan orang lain lebih dulu
	// mendapat nomor ini.
	NextNumber string `json:"next_number"`
	// Version dikirim balik saat menyimpan (Input.Version). 0 selama bawaan
	// yang berlaku.
	Version int `json:"version"`
	// UpdatedAt null selama bawaan yang berlaku.
	UpdatedAt *time.Time `json:"updated_at"`
}

// Listing adalah jawaban GET /document-numbering: yang dibutuhkan layar
// pengaturan penomoran.
type Listing struct {
	// Data: satu skema per jenis dokumen, menurut urutan Options.Types.
	Data []Scheme `json:"data"`
	// Tokens adalah token yang dikenal pola, untuk bantuan di layar.
	Tokens []Token `json:"tokens"`
}

// Input adalah body PUT /document-numbering/{type}. Kedua isian dikirim setiap
// kali.
type Input struct {
	Pattern     string `json:"pattern"`
	ResetPolicy string `json:"reset_policy"`
	// Version adalah Scheme.Version yang dibaca sebelum mengubah; 0 selama
	// bawaan yang berlaku.
	Version int `json:"version"`
}

// normalize merapikan isian di tempat dan mengembalikan yang tidak sah.
func (in *Input) normalize() []appkit.FieldError {
	in.Pattern, in.ResetPolicy = strings.TrimSpace(in.Pattern), strings.TrimSpace(in.ResetPolicy)
	errs := check(in.Pattern, in.ResetPolicy)
	if in.Version < 0 {
		errs = append(errs, appkit.FieldError{Field: "version", Message: "Versi tidak sah."})
	}
	return errs
}

// Nama tindakan di jejak audit, kelompok audit.CategorySettings. Target-nya
// {Type: TargetType, ID: Type.Key}.
const (
	// ActionSchemeUpdated: skema disimpan. Details "fields" menyebut isian yang
	// berubah (`pattern`, `reset_policy`), tanpa nilainya.
	ActionSchemeUpdated = "numbering.scheme_updated"

	TargetType = "document_type"
)

var (
	errUnknownType = appkit.NotFound("Jenis dokumen tidak dikenal.")
	errChanged     = appkit.Conflict("Skema penomoran ini sudah diubah orang lain. Muat ulang halaman, lalu ulangi perubahan Anda.")
)

// begin memeriksa izin Manage, lalu membaca organization request ini.
func (s *Service) begin(ctx context.Context) (uuid.UUID, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return uuid.Nil, err
	}
	return s.hooks.Organization(ctx)
}

// scheme menyusun jawaban API untuk t: e yang berlaku, dan last nomor urut
// terakhir lingkup yang sedang berjalan pada waktu setempat now.
func (t Type) scheme(e effective, now time.Time, last int64) Scheme {
	out := Scheme{
		DocumentType: t.Key, Label: t.Label, Pattern: e.pattern, ResetPolicy: e.reset,
		Custom: e.custom(), Version: e.version, UpdatedAt: e.updatedAt,
	}
	// Pola tersimpan yang rusak tidak menggagalkan layarnya: tanpa layar ini
	// administrator tidak dapat memperbaikinya. Contohnya saja yang kosong.
	if n, err := Format(e.pattern, now, last+1); err == nil {
		out.NextNumber = n
	}
	return out
}

// last membaca nomor urut terakhir lingkup scope; 0 bila belum ada yang
// dipakai.
func last(ctx context.Context, db querier, org uuid.UUID, docType, scope string) (int64, error) {
	var value int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE((
			SELECT last_value FROM appkit_number_counters
			WHERE organization_id = $1 AND document_type = $2 AND scope = $3), 0)`,
		org, docType, scope).Scan(&value)
	if err != nil {
		return 0, fmt.Errorf("numbering: membaca nomor urut %s: %w", docType, err)
	}
	return value, nil
}

// Schemes mengembalikan skema setiap jenis dokumen di organization request
// ini, menurut urutan Options.Types. Jenis yang belum diubah organization itu
// dijawab dengan bawaannya.
func (s *Service) Schemes(ctx context.Context) ([]Scheme, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().In(s.location(ctx, org))

	custom, err := s.customized(ctx, org)
	if err != nil {
		return nil, err
	}
	// Nomor urut dibaca untuk ketiga lingkup yang sedang berjalan sekaligus;
	// tiap jenis dokumen lalu memakai lingkup kebijakannya SENDIRI.
	running := []string{scopeOf(ResetNever, now), scopeOf(ResetYearly, now), scopeOf(ResetMonthly, now)}
	rows, err := s.pool.Query(ctx, `
		SELECT document_type, scope, last_value
		FROM appkit_number_counters
		WHERE organization_id = $1 AND scope = ANY($2)`, org, running)
	if err != nil {
		return nil, fmt.Errorf("numbering: membaca nomor urut: %w", err)
	}
	defer rows.Close()
	type counter struct{ docType, scope string }
	counters := map[counter]int64{}
	for rows.Next() {
		var (
			c     counter
			value int64
		)
		if err := rows.Scan(&c.docType, &c.scope, &value); err != nil {
			return nil, fmt.Errorf("numbering: membaca nomor urut: %w", err)
		}
		counters[c] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("numbering: membaca nomor urut: %w", err)
	}

	out := make([]Scheme, 0, len(s.types))
	for _, t := range s.types {
		e, ok := custom[t.Key]
		if !ok {
			e = t.defaults()
		}
		out = append(out, t.scheme(e, now, counters[counter{t.Key, scopeOf(e.reset, now)}]))
	}
	return out, nil
}

// customized membaca skema yang sudah diubah org, per Type.Key. Baris milik
// jenis dokumen yang tidak lagi terdaftar ikut terbaca dan diabaikan
// pemanggilnya.
func (s *Service) customized(ctx context.Context, org uuid.UUID) (map[string]effective, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT document_type, pattern, reset_policy, version, updated_at
		FROM appkit_number_schemes
		WHERE organization_id = $1`, org)
	if err != nil {
		return nil, fmt.Errorf("numbering: membaca skema: %w", err)
	}
	defer rows.Close()
	out := map[string]effective{}
	for rows.Next() {
		var (
			key string
			e   effective
			at  time.Time
		)
		if err := rows.Scan(&key, &e.pattern, &e.reset, &e.version, &at); err != nil {
			return nil, fmt.Errorf("numbering: membaca skema: %w", err)
		}
		e.updatedAt = &at
		out[key] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("numbering: membaca skema: %w", err)
	}
	return out, nil
}

// SetScheme menyimpan pola dan kebijakan reset jenis dokumen docType untuk
// organization request ini.
//
// in.Version harus sama dengan Version skema yang dibaca (0 selama bawaan yang
// berlaku); bila sudah berubah sejak itu, simpan ditolak dengan galat
// KindConflict. Simpan yang tidak mengubah apa pun tidak menulis apa pun:
// versinya tetap, tidak ada baris yang dibuat, dan tidak ada yang dicatat.
//
// Nomor yang SUDAH terbit tidak disentuh, dan nomor urut tidak pernah kembali
// ke 1 karena skemanya diubah: mengganti pola lalu mengulang dari 1 membuat
// dua dokumen memakai nomor yang sama. Mengganti pola tidak menyentuh nomor
// urut sama sekali; nomor berikutnya melanjutkan urutan yang sedang berjalan
// dengan bentuk baru.
//
// Mengganti KEBIJAKAN RESET memindahkan nomor urut ke lingkup lain — dari
// lingkup tahun ini ke lingkup bulan ini, misalnya. Supaya perpindahan itu
// juga tidak mengulang dari 1, nomor urut yang sedang berjalan dibawa ke
// lingkup barunya: urutan berlanjut, dan baru kembali ke 1 pada pergantian
// masa berikutnya menurut kebijakan yang baru. Yang dibawa hanya lingkup yang
// sedang berjalan saat disimpan; dokumen bertanggal mundur ke masa yang
// dinomori dengan kebijakan lama tidak tercakup, dan tetap dijaga indeks unik
// nomor dokumen milik produk.
func (s *Service) SetScheme(ctx context.Context, docType string, in Input) (Scheme, error) {
	org, err := s.begin(ctx)
	if err != nil {
		return Scheme{}, err
	}
	i, ok := s.byKey[docType]
	if !ok {
		return Scheme{}, errUnknownType
	}
	t := s.types[i]
	if errs := in.normalize(); len(errs) > 0 {
		return Scheme{}, appkit.Validation("Isian belum sesuai.", errs...)
	}
	now := time.Now().In(s.location(ctx, org))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Scheme{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Kunci yang sama dengan Next: dokumen yang sedang disimpan selesai dulu,
	// dan yang datang sesudahnya membaca skema baru. Kunci ini pula yang
	// membuat dua simpan bersamaan bergiliran, termasuk saat barisnya belum ada.
	if err := lock(ctx, tx, org, docType); err != nil {
		return Scheme{}, err
	}
	old, err := s.read(ctx, tx, org, t)
	if err != nil {
		return Scheme{}, err
	}
	if in.Version != old.version {
		return Scheme{}, errChanged
	}

	var fields []string
	if in.Pattern != old.pattern {
		fields = append(fields, "pattern")
	}
	if in.ResetPolicy != old.reset {
		fields = append(fields, "reset_policy")
	}
	e := old
	if len(fields) > 0 {
		if e, err = s.save(ctx, tx, org, t, old, in, now); err != nil {
			return Scheme{}, err
		}
		if err := s.trail.RecordTx(ctx, tx, audit.Entry{
			Category: audit.CategorySettings, Action: ActionSchemeUpdated,
			Target:  audit.Target{Type: TargetType, ID: t.Key},
			Summary: fmt.Sprintf("Penomoran “%s” diubah.", t.Label),
			Details: map[string]any{"fields": fields},
		}); err != nil {
			return Scheme{}, err
		}
	}
	value, err := last(ctx, tx, org, t.Key, scopeOf(e.reset, now))
	if err != nil {
		return Scheme{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Scheme{}, err
	}
	return t.scheme(e, now, value), nil
}

// save menulis skema baru t di tx dan mengembalikan yang kini berlaku. old
// adalah yang berlaku sebelumnya: dibaca di tx yang sama sesudah kunci
// penomorannya diambil, dan sudah dicocokkan versinya.
func (s *Service) save(ctx context.Context, tx pgx.Tx, org uuid.UUID, t Type, old effective, in Input, now time.Time) (effective, error) {
	e := effective{pattern: in.Pattern, reset: in.ResetPolicy}
	var (
		at  time.Time
		row pgx.Row
	)
	// Skema yang masih bawaan belum punya baris; baris pertamanya ber-version 1.
	if old.custom() {
		row = tx.QueryRow(ctx, `
			UPDATE appkit_number_schemes
			SET pattern = $3, reset_policy = $4, version = version + 1, updated_at = now()
			WHERE organization_id = $1 AND document_type = $2
			RETURNING version, updated_at`, org, t.Key, e.pattern, e.reset)
	} else {
		row = tx.QueryRow(ctx, `
			INSERT INTO appkit_number_schemes (organization_id, document_type, pattern, reset_policy)
			VALUES ($1, $2, $3, $4)
			RETURNING version, updated_at`, org, t.Key, e.pattern, e.reset)
	}
	if err := row.Scan(&e.version, &at); err != nil {
		return effective{}, fmt.Errorf("numbering: menyimpan skema %s: %w", t.Key, err)
	}
	e.updatedAt = &at

	// Kebijakan reset berganti: nomor urut yang sedang berjalan dibawa ke
	// lingkup barunya (lihat SetScheme). Lingkup baru yang sudah pernah dipakai
	// tidak pernah diturunkan.
	if from, to := scopeOf(old.reset, now), scopeOf(e.reset, now); from != to {
		if _, err := tx.Exec(ctx, `
			INSERT INTO appkit_number_counters (organization_id, document_type, scope, last_value)
			SELECT organization_id, document_type, $4, last_value
			FROM appkit_number_counters
			WHERE organization_id = $1 AND document_type = $2 AND scope = $3
			ON CONFLICT (organization_id, document_type, scope)
			DO UPDATE SET last_value = GREATEST(appkit_number_counters.last_value, EXCLUDED.last_value)`,
			org, t.Key, from, to); err != nil {
			return effective{}, fmt.Errorf("numbering: melanjutkan nomor urut %s: %w", t.Key, err)
		}
	}
	return e, nil
}
