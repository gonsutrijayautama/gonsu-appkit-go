package roles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// Listing adalah jawaban GET /roles: yang dibutuhkan layar penyusun role.
type Listing struct {
	// Roles: role bawaan menurut urutan Options.Builtins, lalu role buatan
	// urut nama.
	Roles []Role `json:"roles"`
	// Users: jumlah pengguna per Role.Key, dari Options.UserCounts. Setiap
	// role di Roles punya isiannya.
	Users map[string]int `json:"users"`
	// Permissions adalah katalog izin, dalam urutan tampilnya.
	Permissions []Definition `json:"permissions"`
	Custom      Custom       `json:"custom"`
}

// Custom menyebut apakah role buatan dapat dipakai organization ini, dan
// berapa yang sudah dibuat.
type Custom struct {
	// Enabled false: paketnya tidak menyertakan role buatan.
	Enabled bool `json:"enabled"`
	Count   int  `json:"count"`
	Max     int  `json:"max"`
}

// Input adalah body POST /roles dan PUT /roles/{key}. Seluruh isian dikirim
// setiap kali; yang tidak dikirim dianggap kosong.
type Input struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Audience kosong berarti AudienceInternal saat membuat. Saat mengubah ia
	// boleh kosong, dan bila diisi harus sama dengan audiens role itu.
	Audience    Audience            `json:"audience"`
	Permissions []appkit.Permission `json:"permissions"`
	// Version adalah Role.Version yang dibaca sebelum mengubah. Tidak dipakai
	// saat membuat.
	Version int `json:"version"`
}

// Batas isi, sama dengan constraint kolomnya.
const (
	maxName        = 60
	maxDescription = 200
)

// normalize merapikan isian di tempat dan mengembalikan yang tidak sah.
// in.Audience harus sudah terisi.
func (s *Service) normalize(in *Input) []appkit.FieldError {
	var errs []appkit.FieldError
	fail := func(field, message string) {
		errs = append(errs, appkit.FieldError{Field: field, Message: message})
	}

	if in.Version < 0 {
		fail("version", "Versi tidak sah.")
	}

	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "":
		fail("name", "Nama role wajib diisi.")
	case utf8.RuneCountInString(in.Name) > maxName:
		fail("name", fmt.Sprintf("Nama role maksimal %d karakter.", maxName))
	case s.builtinNamed(in.Name):
		fail("name", "Nama ini sudah dipakai role bawaan.")
	}

	in.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(in.Description) > maxDescription {
		fail("description", fmt.Sprintf("Keterangan maksimal %d karakter.", maxDescription))
	}

	if !in.Audience.forRole() {
		fail("audience", "Pilih role internal atau role untuk orang luar.")
		return errs
	}
	perms, problem := s.check(in.Permissions, in.Audience)
	switch {
	case problem != "":
		fail("permissions", problem)
	case len(perms) == 0:
		fail("permissions", "Pilih minimal satu izin.")
	}
	in.Permissions = perms
	return errs
}

func (in Input) snapshot() *Snapshot {
	return &Snapshot{Name: in.Name, Description: in.Description, Audience: in.Audience, Permissions: in.Permissions}
}

func names(perms []appkit.Permission) []string {
	out := make([]string, len(perms))
	for i, p := range perms {
		out[i] = string(p)
	}
	return out
}

var (
	errChanged   = appkit.Conflict("Role ini sudah diubah orang lain. Muat ulang halaman, lalu ulangi perubahan Anda.")
	errNameTaken = appkit.Validation("Isian belum sesuai.", appkit.FieldError{Field: "name", Message: "Nama ini sudah dipakai role lain."})
	errDisabled  = appkit.QuotaExceeded("Paket Anda tidak menyertakan role buatan.")
	errAudience  = appkit.Validation("Isian belum sesuai.", appkit.FieldError{Field: "audience", Message: "Audiens role tidak dapat diubah setelah role dibuat."})
)

// Kode galat PostgreSQL.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

func violates(err error, code string) bool {
	e, ok := errors.AsType[*pgconn.PgError](err)
	return ok && e.Code == code
}

// begin memeriksa izin Manage, lalu membaca organization dan pelaku request
// ini.
func (s *Service) begin(ctx context.Context) (org, actor uuid.UUID, err error) {
	if err = s.hooks.Authorize(ctx, Manage); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if org, err = s.hooks.Organization(ctx); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if actor, err = s.hooks.User(ctx); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return org, actor, nil
}

// requireCustom menolak bila paket org tidak menyertakan role buatan.
func (s *Service) requireCustom(ctx context.Context, org uuid.UUID) error {
	ok, err := s.enabled(ctx, org)
	if err != nil {
		return err
	}
	if !ok {
		return errDisabled
	}
	return nil
}

// List mengembalikan role organization request ini beserta katalog izinnya.
func (s *Service) List(ctx context.Context) (Listing, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return Listing{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Listing{}, err
	}
	active, err := s.enabled(ctx, org)
	if err != nil {
		return Listing{}, err
	}
	all, err := s.all(ctx, org, active)
	if err != nil {
		return Listing{}, err
	}
	counts, err := s.opts.UserCounts(ctx, org)
	if err != nil {
		return Listing{}, fmt.Errorf("roles: menghitung pemegang role: %w", err)
	}
	out := Listing{
		Roles: all, Users: make(map[string]int, len(all)), Permissions: slices.Clone(s.catalog),
		Custom: Custom{Enabled: active, Count: len(all) - len(s.builtins), Max: s.opts.MaxCustom},
	}
	for _, r := range all {
		out.Users[r.Key] = counts[r.Key]
	}
	return out, nil
}

// Create membuat role buatan. Galatnya berjenis appkit.KindQuotaExceeded bila
// paket organization itu tidak menyertakan role buatan.
func (s *Service) Create(ctx context.Context, in Input) (Role, error) {
	org, actor, err := s.begin(ctx)
	if err != nil {
		return Role{}, err
	}
	if err := s.requireCustom(ctx, org); err != nil {
		return Role{}, err
	}
	if in.Audience == "" {
		in.Audience = AudienceInternal
	}
	if errs := s.normalize(&in); len(errs) > 0 {
		return Role{}, appkit.Validation("Isian belum lengkap.", errs...)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Role{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Dua pembuatan bersamaan di satu organization bergiliran, supaya batas
	// jumlahnya tidak terlewati. Kuncinya tingkat TRANSAKSI: kunci tingkat
	// sesi tidak menjaga apa pun di belakang PgBouncer `pool_mode=transaction`.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "appkit_roles:"+org.String()); err != nil {
		return Role{}, fmt.Errorf("roles: mengunci pembuatan role: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM appkit_roles WHERE organization_id = $1`, org).Scan(&count); err != nil {
		return Role{}, fmt.Errorf("roles: menghitung role: %w", err)
	}
	if count >= s.opts.MaxCustom {
		return Role{}, appkit.Validation(fmt.Sprintf(
			"Jumlah role buatan sudah mencapai batas (%d). Hapus role yang tidak dipakai lebih dulu.", s.opts.MaxCustom))
	}

	r := stored{
		id: uuid.New(), name: in.Name, description: in.Description, audience: in.Audience,
		permissions: names(in.Permissions), version: 1,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO appkit_roles (id, organization_id, name, description, audience, permissions)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING updated_at`,
		r.id, org, r.name, r.description, string(r.audience), r.permissions).Scan(&r.updatedAt)
	if violates(err, uniqueViolation) {
		return Role{}, errNameTaken
	}
	if err != nil {
		return Role{}, fmt.Errorf("roles: menyimpan role: %w", err)
	}
	if err := record(ctx, tx, org, r.id, actor, ActionCreated, nil, in.snapshot()); err != nil {
		return Role{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Role{}, err
	}
	return s.role(r, true), nil
}

// Update menyimpan seluruh isian role buatan key (bukan sebagian). Audiensnya
// tidak dapat diubah.
//
// in.Version harus sama dengan Version role yang dibaca; bila sudah berubah
// sejak itu, simpan ditolak dengan galat KindConflict.
func (s *Service) Update(ctx context.Context, key string, in Input) (Role, error) {
	org, actor, err := s.begin(ctx)
	if err != nil {
		return Role{}, err
	}
	if _, ok := s.byKey[key]; ok {
		return Role{}, errBuiltin
	}
	id, ok := customID(key)
	if !ok {
		return Role{}, errNotFound
	}
	if err := s.requireCustom(ctx, org); err != nil {
		return Role{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Role{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Baris dikunci: isi lama yang dicatat adalah isi yang benar-benar diganti.
	r, err := scanRole(tx.QueryRow(ctx, selectRole+` WHERE organization_id = $1 AND id = $2 FOR UPDATE`, org, id))
	if err != nil {
		return Role{}, err
	}
	if in.Audience != "" && in.Audience != r.audience {
		return Role{}, errAudience
	}
	in.Audience = r.audience
	if errs := s.normalize(&in); len(errs) > 0 {
		return Role{}, appkit.Validation("Isian belum lengkap.", errs...)
	}
	if in.Version != r.version {
		return Role{}, errChanged
	}

	before := r.snapshot()
	r.name, r.description, r.permissions = in.Name, in.Description, names(in.Permissions)
	err = tx.QueryRow(ctx, `
		UPDATE appkit_roles
		SET name = $3, description = $4, permissions = $5, version = version + 1, updated_at = now()
		WHERE organization_id = $1 AND id = $2
		RETURNING version, updated_at`,
		org, id, r.name, r.description, r.permissions).Scan(&r.version, &r.updatedAt)
	if violates(err, uniqueViolation) {
		return Role{}, errNameTaken
	}
	if err != nil {
		return Role{}, fmt.Errorf("roles: menyimpan role: %w", err)
	}
	if err := record(ctx, tx, org, id, actor, ActionUpdated, before, in.snapshot()); err != nil {
		return Role{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Role{}, err
	}
	return s.role(r, true), nil
}

// Delete menghapus role buatan key. Role yang masih dipegang pengguna
// ditolak. Menghapus tetap boleh di organization yang paketnya tidak lagi
// menyertakan role buatan.
//
// Pemeriksaan pemegangnya tidak atomik dengan pemberian role di produk: role
// yang diberikan tepat saat dihapus menjadi Key tanpa role, yang tidak
// memegang izin apa pun (PermissionsOf). Produk yang ingin database
// menolaknya memasang foreign key ke appkit_roles (organization_id, id);
// pelanggarannya dijawab dengan galat yang sama.
func (s *Service) Delete(ctx context.Context, key string) error {
	org, actor, err := s.begin(ctx)
	if err != nil {
		return err
	}
	if _, ok := s.byKey[key]; ok {
		return errBuiltin
	}
	id, ok := customID(key)
	if !ok {
		return errNotFound
	}
	counts, err := s.opts.UserCounts(ctx, org)
	if err != nil {
		return fmt.Errorf("roles: menghitung pemegang role: %w", err)
	}
	if n := counts[key]; n > 0 {
		return appkit.Validation(fmt.Sprintf(
			"Role ini masih dipakai %d pengguna. Pindahkan mereka ke role lain lebih dulu.", n))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r := stored{id: id}
	var audience string
	err = tx.QueryRow(ctx, `
		DELETE FROM appkit_roles WHERE organization_id = $1 AND id = $2
		RETURNING name, description, audience, permissions`, org, id).
		Scan(&r.name, &r.description, &audience, &r.permissions)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errNotFound
	case violates(err, foreignKeyViolation):
		return appkit.Validation("Role ini masih dipakai pengguna. Pindahkan mereka ke role lain lebih dulu.")
	case err != nil:
		return fmt.Errorf("roles: menghapus role: %w", err)
	}
	r.audience = Audience(audience)
	if err := record(ctx, tx, org, id, actor, ActionDeleted, r.snapshot(), nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Tindakan yang dicatat.
const (
	ActionCreated = "created"
	ActionUpdated = "updated"
	ActionDeleted = "deleted"
)

// Snapshot adalah isi sebuah role pada satu saat.
type Snapshot struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Audience    Audience            `json:"audience"`
	Permissions []appkit.Permission `json:"permissions"`
}

// Event adalah satu catatan perubahan role.
type Event struct {
	ID uuid.UUID `json:"id"`
	// Action: ActionCreated, ActionUpdated, atau ActionDeleted.
	Action string `json:"action"`
	// RoleKey tetap terisi setelah rolenya dihapus.
	RoleKey string `json:"role_key"`
	// RoleName adalah nama role sesudah perubahan; untuk penghapusan, nama
	// terakhirnya.
	RoleName string `json:"role_name"`
	// ActorID adalah id pengguna di dalam produk (Hooks.User).
	ActorID uuid.UUID `json:"actor_id"`
	// Before null untuk pembuatan; After null untuk penghapusan.
	Before    *Snapshot `json:"before"`
	After     *Snapshot `json:"after"`
	CreatedAt time.Time `json:"created_at"`
}

// record mencatat satu perubahan di transaksi yang sama dengan perubahannya:
// perubahan tanpa catatan tidak pernah tersimpan.
func record(ctx context.Context, tx pgx.Tx, org, role, actor uuid.UUID, action string, before, after *Snapshot) error {
	states := [2][]byte{}
	for i, state := range []*Snapshot{before, after} {
		if state == nil {
			continue
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		states[i] = raw
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO appkit_role_events (id, organization_id, role_id, action, actor_id, old_state, new_state)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, org, role, action, actor, states[0], states[1]); err != nil {
		return fmt.Errorf("roles: mencatat perubahan role: %w", err)
	}
	return nil
}

const (
	defaultEvents = 50
	maxEvents     = 200
)

// Events mengembalikan catatan perubahan role organization request ini,
// terbaru dulu. limit di bawah satu memakai bawaan (50); paling banyak 200.
func (s *Service) Events(ctx context.Context, limit int) ([]Event, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return nil, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = defaultEvents
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, role_id, action, actor_id, old_state, new_state, created_at
		FROM appkit_role_events
		WHERE organization_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, org, min(limit, maxEvents))
	if err != nil {
		return nil, fmt.Errorf("roles: membaca catatan perubahan: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var (
			e          Event
			role       uuid.UUID
			old, fresh []byte
		)
		if err := rows.Scan(&e.ID, &role, &e.Action, &e.ActorID, &old, &fresh, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("roles: membaca catatan perubahan: %w", err)
		}
		e.RoleKey = role.String()
		for _, state := range []struct {
			raw    []byte
			target **Snapshot
		}{{old, &e.Before}, {fresh, &e.After}} {
			if state.raw == nil {
				continue
			}
			var snap Snapshot
			if err := json.Unmarshal(state.raw, &snap); err != nil {
				return nil, fmt.Errorf("roles: catatan perubahan %s rusak: %w", e.ID, err)
			}
			*state.target = &snap
			e.RoleName = snap.Name
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("roles: membaca catatan perubahan: %w", err)
	}
	return out, nil
}
