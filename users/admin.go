package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/roles"
)

// Layar Pengguna & Akses. Seluruh tindakannya menuntut izin Manage.

// View adalah satu pengguna di layar.
type View struct {
	User
	// IsSelf menandai pengguna yang sedang membuka layar: ia tidak dapat
	// menonaktifkan dirinya sendiri.
	IsSelf bool `json:"is_self"`
}

// Listing adalah jawaban GET /users.
type Listing struct {
	Data []View `json:"data"`
	// Invite menjawab apakah orang baru dapat ditambahkan dari layar ini.
	Invite Invite `json:"invite"`
	Seats  Seats  `json:"seats"`
	// Roles adalah seluruh role organization ini, untuk menampilkan nama role
	// tiap pengguna dan mengisi pilihannya. Yang dapat dipilih di layar ini
	// hanya yang beraudiens internal.
	Roles []roles.Role `json:"roles"`
}

// Invite menyebut kesiapan pemberian akses dari layar.
type Invite struct {
	Available bool `json:"available"`
	// Reason terisi hanya bila Available false.
	Reason string `json:"reason,omitempty"`
}

// Seats adalah pemakaian batas pengguna.
type Seats struct {
	Active int64 `json:"active"`
	// Max null berarti tanpa batas.
	Max *int64 `json:"max"`
}

var errUnavailable = appkit.Validation("Pemasangan ini belum dapat menambah orang baru dari layar ini.")

// begin memeriksa izin Manage, lalu membaca organization dan pengguna request
// ini.
func (s *Service) begin(ctx context.Context) (org, caller uuid.UUID, err error) {
	if err = s.hooks.Authorize(ctx, Manage); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if org, err = s.hooks.Organization(ctx); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if caller, err = s.hooks.User(ctx); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return org, caller, nil
}

func (s *Service) available(ctx context.Context) bool {
	if s.opts.Provision == nil {
		return false
	}
	return s.opts.Available == nil || s.opts.Available(ctx)
}

func view(u User, caller uuid.UUID) View { return View{User: u, IsSelf: u.ID == caller} }

// List mendaftar pengguna organization request ini beserta batas pengguna,
// kesiapan pemberian akses, dan role-nya.
func (s *Service) List(ctx context.Context) (Listing, error) {
	org, caller, err := s.begin(ctx)
	if err != nil {
		return Listing{}, err
	}
	all, err := s.All(ctx, org)
	if err != nil {
		return Listing{}, err
	}
	out := Listing{Data: make([]View, 0, len(all))}
	for _, u := range all {
		out.Data = append(out.Data, view(u, caller))
		if u.Status == StatusActive {
			out.Seats.Active++
		}
	}
	if s.opts.Seats != nil {
		limit, err := s.opts.Seats(ctx, org)
		if err != nil {
			return Listing{}, fmt.Errorf("users: membaca batas pengguna: %w", err)
		}
		if limit >= 0 {
			out.Seats.Max = &limit
		}
	}
	if out.Invite.Available = s.available(ctx); !out.Invite.Available {
		out.Invite.Reason = errUnavailable.Message
	}
	if out.Roles, err = s.access.All(ctx, org); err != nil {
		return Listing{}, err
	}
	return out, nil
}

// InviteInput adalah body POST /users.
type InviteInput struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	// Role adalah roles.Role.Key beraudiens internal.
	Role string `json:"role"`
}

// Akun login orang yang diberi akses.
const (
	// AccountCreated: akunnya baru dibuat; TemporaryPassword terisi.
	AccountCreated = "created"
	// AccountExisting: orangnya sudah punya akun.
	AccountExisting = "existing"
)

// Invited adalah jawaban POST /users. TemporaryPassword hanya ada ketika
// akunnya baru dibuat; ia tampil sekali di layar dan tidak disimpan maupun
// dicatat.
type Invited struct {
	User              View   `json:"user"`
	Account           string `json:"account"`
	TemporaryPassword string `json:"temporary_password,omitempty"`
}

// internal memastikan role key ada dan beraudiens internal. Layar ini untuk
// staf: orang luar butuh ikatan ke pihaknya (mis. pelanggannya), yang hanya
// diketahui produk, jadi aksesnya diberikan alur produk lewat Grant.
func (s *Service) internal(ctx context.Context, org uuid.UUID, key string) (roles.Role, *appkit.FieldError) {
	r, err := s.access.Get(ctx, org, key)
	switch {
	case err != nil:
		return roles.Role{}, &appkit.FieldError{Field: "role", Message: "Pilih role yang tersedia."}
	case r.Audience != roles.AudienceInternal:
		return roles.Role{}, &appkit.FieldError{Field: "role", Message: "Role untuk orang luar tidak diberikan dari layar ini."}
	}
	return r, nil
}

// Invite memberi akses kepada satu orang lewat emailnya: penyedia identitas
// membuatkan (atau menemukan) akunnya, lalu orangnya dicatat sebagai pengguna.
//
// Jangan dilewatkan modul idempotency: jawabannya dapat membawa sandi
// sementara, dan jawaban yang diputar ulang akan menyimpannya di database.
// Permintaan yang diulang tetap aman karena penyedia identitas mengembalikan
// orang yang sama tanpa sandi baru.
func (s *Service) Invite(ctx context.Context, in InviteInput) (Invited, error) {
	org, caller, err := s.begin(ctx)
	if err != nil {
		return Invited{}, err
	}
	in.Email = strings.TrimSpace(in.Email)
	in.Name = clean(in.Name)
	var errs []appkit.FieldError
	if at := strings.IndexByte(in.Email, '@'); at < 1 || at == len(in.Email)-1 ||
		strings.ContainsAny(in.Email, " \t\r\n") || utf8.RuneCountInString(in.Email) > maxEmail {
		errs = append(errs, appkit.FieldError{Field: "email", Message: "Isi alamat email yang valid."})
	}
	switch {
	case in.Name == "":
		errs = append(errs, appkit.FieldError{Field: "name", Message: "Nama wajib diisi."})
	case utf8.RuneCountInString(in.Name) > maxName:
		errs = append(errs, appkit.FieldError{Field: "name", Message: fmt.Sprintf("Nama maksimal %d karakter.", maxName)})
	}
	role, problem := s.internal(ctx, org, in.Role)
	if problem != nil {
		errs = append(errs, *problem)
	}
	if len(errs) > 0 {
		return Invited{}, appkit.Validation("Isian belum lengkap.", errs...)
	}
	if !s.available(ctx) {
		return Invited{}, errUnavailable
	}
	// Batas diperiksa sebelum akunnya dibuat: akun yang tidak dapat diberi
	// akses hanya menghabiskan jatah pembuatan akun pemasangan. Grant
	// memeriksanya lagi di dalam kunci.
	if err := s.seats(ctx, s.pool, org); err != nil {
		return Invited{}, err
	}

	id, err := s.opts.Provision(ctx, in.Email, in.Name)
	if err != nil {
		return Invited{}, err
	}
	if strings.TrimSpace(id.Subject) == "" {
		return Invited{}, errors.New("users: Options.Provision mengembalikan akun tanpa Subject")
	}
	existing, err := s.BySubject(ctx, org, id.Subject)
	if err == nil && existing.Status == StatusActive {
		msg := fmt.Sprintf("Orang ini sudah punya akses di sini sebagai %s. Ubah role-nya dari daftar pengguna.", s.roleName(ctx, org, existing.Role))
		return Invited{}, appkit.Validation(msg, appkit.FieldError{Field: "email", Message: msg})
	}
	if err != nil && kind(err) != appkit.KindNotFound {
		return Invited{}, err
	}

	u, err := s.Grant(ctx, org, GrantInput{
		Subject: id.Subject, Email: orDefault(strings.TrimSpace(id.Email), in.Email), Name: orDefault(clean(id.Name), in.Name),
		Role: role.Key, Actor: Actor{UserID: caller, Source: SourceScreen},
	})
	if err != nil {
		return Invited{}, err
	}
	out := Invited{User: view(u, caller), Account: AccountExisting, TemporaryPassword: id.TemporaryPassword}
	if id.TemporaryPassword != "" {
		out.Account = AccountCreated
	}
	return out, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// UpdateInput adalah body PATCH /users/{id}. Isian yang kosong tidak diubah.
type UpdateInput struct {
	// Role adalah roles.Role.Key.
	Role string `json:"role"`
	// Status: StatusActive atau StatusSuspended.
	Status string `json:"status"`
}

// Update mengubah role atau status satu pengguna.
//
// Pengaman: tidak ada yang dapat menonaktifkan dirinya sendiri; administrator
// aktif terakhir tidak dapat diturunkan atau dinonaktifkan — organization
// tanpa administrator tidak dapat memberi akses kepada siapa pun lagi selain
// lewat operator; dan role pengganti harus seaudiens dengan role sekarang.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (View, error) {
	org, caller, err := s.begin(ctx)
	if err != nil {
		return View{}, err
	}
	var errs []appkit.FieldError
	if in.Role == "" && in.Status == "" {
		errs = append(errs, appkit.FieldError{Field: "role", Message: "Sebutkan role atau status yang diubah."})
	}
	if in.Status != "" && in.Status != StatusActive && in.Status != StatusSuspended {
		errs = append(errs, appkit.FieldError{Field: "status", Message: "Status harus active atau suspended."})
	}
	if len(errs) > 0 {
		return View{}, appkit.Validation("Perubahan belum sesuai.", errs...)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return View{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.LockGrants(ctx, tx, org); err != nil {
		return View{}, err
	}
	cur, err := scanUser(tx.QueryRow(ctx, selectUser+` WHERE organization_id = $1 AND id = $2 FOR UPDATE`, org, id))
	if err != nil {
		return View{}, err
	}

	role, status := cur.Role, cur.Status
	if in.Status != "" {
		status = in.Status
	}
	if in.Role != "" && in.Role != cur.Role {
		// Dibaca sesudah kunci diambil: lihat Grant.
		next, err := s.access.Get(ctx, org, in.Role)
		if err != nil {
			if kind(err) == appkit.KindNotFound {
				return View{}, errRole
			}
			return View{}, err
		}
		// Role sekarang yang sudah terhapus diperlakukan sebagai internal.
		audience := roles.AudienceInternal
		if now, err := s.access.Get(ctx, org, cur.Role); err == nil {
			audience = now.Audience
		} else if kind(err) != appkit.KindNotFound {
			return View{}, err
		}
		if next.Audience != audience {
			msg := "Role ini untuk jenis pengguna yang berbeda."
			return View{}, appkit.Validation(msg, appkit.FieldError{Field: "role", Message: msg})
		}
		role = next.Key
	}

	if status == StatusSuspended && cur.Status == StatusActive && id == caller {
		msg := "Anda tidak dapat menonaktifkan akun Anda sendiri."
		return View{}, appkit.Validation(msg, appkit.FieldError{Field: "status", Message: msg})
	}
	administrator := s.access.Administrator().Key
	if cur.Status == StatusActive && cur.Role == administrator && (status != StatusActive || role != administrator) {
		var others int
		if err := tx.QueryRow(ctx, `
			SELECT count(*)::int FROM appkit_users
			WHERE organization_id = $1 AND id <> $2 AND status = 'active' AND role_key = $3`,
			org, id, administrator).Scan(&others); err != nil {
			return View{}, fmt.Errorf("users: menghitung administrator: %w", err)
		}
		if others == 0 {
			msg := "Harus tersisa minimal satu administrator aktif."
			return View{}, appkit.Validation(msg, appkit.FieldError{Field: "role", Message: msg})
		}
	}

	a := Actor{UserID: caller, Source: SourceScreen}
	changeRole := func() error {
		if err := setRole(ctx, tx, org, id, role); err != nil {
			return err
		}
		// Role baru berlaku pada permintaan berikutnya selama produk membaca
		// role dari pengguna setiap kali; tidak ada sesi yang perlu dicabut.
		return s.record(ctx, tx, org, a, ActionRoleChanged, cur,
			fmt.Sprintf("Role %s diubah menjadi %s.", cur.label(), s.roleName(ctx, org, role)),
			map[string]any{"role_before": cur.Role, "role_after": role})
	}
	switch {
	case status == StatusSuspended && cur.Status == StatusActive:
		if role != cur.Role {
			if err := changeRole(); err != nil {
				return View{}, err
			}
		}
		next := cur
		next.Role = role
		if err := s.suspend(ctx, tx, org, next, a); err != nil {
			return View{}, err
		}
	case status == StatusActive && cur.Status == StatusSuspended:
		if err := s.seats(ctx, tx, org); err != nil {
			return View{}, err
		}
		if err := setRole(ctx, tx, org, id, role); err != nil {
			return View{}, err
		}
		if err := setStatus(ctx, tx, org, id, StatusActive); err != nil {
			return View{}, err
		}
		if err := s.record(ctx, tx, org, a, ActionReactivated, cur,
			fmt.Sprintf("Akses %s diaktifkan kembali sebagai %s.", cur.label(), s.roleName(ctx, org, role)),
			map[string]any{"role_before": cur.Role, "role_after": role}); err != nil {
			return View{}, err
		}
	case role != cur.Role:
		if err := changeRole(); err != nil {
			return View{}, err
		}
	}

	out, err := scanUser(tx.QueryRow(ctx, selectUser+` WHERE organization_id = $1 AND id = $2`, org, id))
	if err != nil {
		return View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, err
	}
	return view(out, caller), nil
}
