// Package users mengelola siapa yang berhak masuk ke sebuah organization, dan
// dengan role apa.
//
// Pengguna lahir dari pemberian akses yang disengaja, tidak pernah dari login
// pertama: orang yang tidak dikenal ditolak, bukan dibuatkan akun. Tiga jalan,
// satu aturan:
//
//   - layar Pengguna & Akses (Invite, Update), di balik izin Manage;
//   - perintah operator (Grant, Suspend), tanpa sesi;
//   - pemilik organization saat masuk pertama (Grant), tanpa sesi.
//
// Setiap perubahan akses dicatat di jejak audit, di transaksi yang sama.
//
// Yang TIDAK ada di sini:
//
//   - sandi. Tidak ada kolomnya dan tidak boleh ditambahkan;
//   - login dan sesi. Keduanya milik produk; yang dipakai dari sini hanya
//     BySubject, ByID, dan RecordLogin. Mencabut sesi orang yang dinonaktifkan
//     diminta lewat Options.RevokeSessions;
//   - pembuatan akun di penyedia identitas. Library ini tidak pernah memanggil
//     platform; produk menyuntikkannya lewat Options.Provision;
//   - kuota. Batas pengguna aktif diminta lewat Options.Seats — biasanya nilai
//     hak pakai paketnya — dan ditegakkan di setiap pemberian akses.
//
// Pengaman yang ditegakkan di sini: tidak ada yang dapat menonaktifkan dirinya
// sendiri, administrator aktif terakhir tidak dapat diturunkan atau
// dinonaktifkan lewat layar, role yang diberikan harus ada (package roles),
// dan jenis orangnya tidak berubah — staf tidak menjadi orang luar, atau
// sebaliknya, dengan mengganti role.
package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/roles"
)

// Manage adalah izin memberi akses, mengubah role, dan menonaktifkan pengguna.
// Tandai Sensitive di katalog izin: pemegangnya dapat memperluas akses orang
// lain.
const Manage appkit.Permission = "settings.users.manage"

// Status pengguna.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
)

// Jalan sebuah perubahan akses, dicatat di jejak audit.
const (
	// SourceScreen: layar Pengguna & Akses.
	SourceScreen = "screen"
	// SourceOperator: perintah operator pemasangan.
	SourceOperator = "operator"
	// SourceOwner: pemilik organization saat masuk pertama. Grant dengan jalan
	// ini hanya berhasil bila organization BELUM PERNAH memberi akses kepada
	// siapa pun; sesudah itu ErrBootstrapped.
	SourceOwner = "owner"
)

// Unlimited adalah jawaban Options.Seats untuk organization tanpa batas
// pengguna.
const Unlimited int64 = -1

// Identity adalah akun login seseorang di penyedia identitas produk.
type Identity struct {
	// Subject adalah pengenal tetap orang itu (klaim `sub`). Wajib.
	Subject string
	// Email dan Name menurut penyedia identitas; kosong berarti pakai yang
	// diketik di layar.
	Email string
	Name  string
	// TemporaryPassword hanya terisi bila akunnya BARU dibuat. Ia tampil
	// sekali di layar dan tidak pernah disimpan atau dicatat di sini.
	TemporaryPassword string
}

// Options mengatur Service. RevokeSessions wajib.
type Options struct {
	// Seats mengembalikan batas pengguna AKTIF org — biasanya nilai hak pakai
	// paketnya. Kosong: tanpa batas untuk semua.
	//
	// Unlimited (atau nilai negatif apa pun) berarti tanpa batas. NOL berarti
	// tidak boleh ada pengguna aktif baru, BUKAN tanpa batas: hak pakai yang
	// tidak dibawa paket dijawab nol, dan itu tidak boleh membuka pintu.
	//
	// Yang dihitung seluruh pengguna aktif, apa pun rolenya.
	Seats func(ctx context.Context, org uuid.UUID) (limit int64, err error)
	// Provision membuatkan — atau menemukan — akun login untuk email itu di
	// penyedia identitas produk. Galatnya diteruskan apa adanya ke WriteError,
	// jadi produk bebas memakai tipe galatnya sendiri. Kosong: orang baru tidak
	// dapat ditambahkan dari layar; Grant tetap berjalan.
	Provision func(ctx context.Context, email, name string) (Identity, error)
	// Available menjawab apakah Provision dapat dipakai SEKARANG: pemasangan
	// yang belum menerima jalan pemberian akses menjawab false, dan layar
	// mengatakannya terus terang alih-alih gagal saat tombol ditekan. Kosong:
	// tersedia selama Provision terisi.
	Available func(ctx context.Context) bool
	// RevokeSessions mencabut seluruh sesi berjalan milik user, di transaksi
	// tx: orang yang dinonaktifkan kehilangan aksesnya bersama perubahan
	// statusnya, bukan saat sesinya habis. Sesi adalah tabel milik produk.
	RevokeSessions func(ctx context.Context, tx pgx.Tx, org, user uuid.UUID) error
}

// Service mengelola pengguna.
type Service struct {
	pool   *pgxpool.Pool
	access *roles.Service
	trail  *audit.Service
	hooks  appkit.Hooks
	opts   Options
}

// New mengembalikan service pengguna. Role diperiksa lewat access, dan setiap
// perubahan akses dicatat lewat trail.
func New(pool *pgxpool.Pool, access *roles.Service, trail *audit.Service, hooks appkit.Hooks, opts Options) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("users: pool wajib diisi")
	case access == nil:
		return nil, errors.New("users: service role wajib diisi")
	case trail == nil:
		return nil, errors.New("users: service jejak audit wajib diisi")
	case opts.RevokeSessions == nil:
		return nil, errors.New("users: Options.RevokeSessions wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	if hooks.User == nil {
		return nil, errors.New("users: Hooks.User wajib diisi")
	}
	return &Service{pool: pool, access: access, trail: trail, hooks: hooks, opts: opts}, nil
}

// User adalah satu pengguna.
type User struct {
	ID uuid.UUID `json:"id"`
	// Subject adalah pengenal orangnya di penyedia identitas (klaim `sub`).
	Subject string `json:"subject"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	// Role adalah roles.Role.Key.
	Role string `json:"role"`
	// Status: StatusActive atau StatusSuspended.
	Status      string     `json:"status"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

// label adalah sebutan orangnya di kalimat jejak audit.
func (u User) label() string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return u.Subject
}

// querier dipenuhi *pgxpool.Pool dan pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const columns = `id, external_subject, email, name, role_key, status, last_login_at, created_at`

const selectUser = `SELECT ` + columns + ` FROM appkit_users`

var errNotFound = appkit.NotFound("Pengguna tidak ditemukan.")

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Subject, &u.Email, &u.Name, &u.Role, &u.Status, &u.LastLoginAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, errNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("users: membaca pengguna: %w", err)
	}
	return u, nil
}

// All mengembalikan seluruh pengguna org, urut waktu diberi akses, TANPA sesi
// dan tanpa memeriksa izin: untuk kode server produk. org ditentukan kode
// server, tidak pernah dari body atau query.
func (s *Service) All(ctx context.Context, org uuid.UUID) ([]User, error) {
	return list(ctx, s.pool, org)
}

func list(ctx context.Context, db querier, org uuid.UUID) ([]User, error) {
	rows, err := db.Query(ctx, selectUser+` WHERE organization_id = $1 ORDER BY created_at, id`, org)
	if err != nil {
		return nil, fmt.Errorf("users: membaca pengguna: %w", err)
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("users: membaca pengguna: %w", err)
	}
	return out, nil
}

// BySubject membaca pengguna org dengan pengenal subject, tanpa sesi: untuk
// login produk. Orang yang tidak pernah diberi akses di org itu — termasuk
// yang punya akses di organization lain — dijawab "tidak ditemukan".
// Pemanggil memeriksa Status: yang dinonaktifkan tidak boleh masuk.
func (s *Service) BySubject(ctx context.Context, org uuid.UUID, subject string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, selectUser+` WHERE organization_id = $1 AND external_subject = $2`, org, subject))
}

// ByID membaca pengguna id milik org, tanpa sesi: untuk produk saat memeriksa
// sesi pada setiap permintaan. Pemanggil memeriksa Status.
func (s *Service) ByID(ctx context.Context, org, id uuid.UUID) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, selectUser+` WHERE organization_id = $1 AND id = $2`, org, id))
}

// ActorName mengembalikan nama tampil pengguna id di org — nama, atau email
// bila nama kosong — dan string kosong bila tidak ditemukan. Bentuknya sesuai
// audit.Options.ActorName.
func (s *Service) ActorName(ctx context.Context, org, id uuid.UUID) string {
	u, err := s.ByID(ctx, org, id)
	if err != nil {
		return ""
	}
	return u.label()
}

// ActionSignedIn adalah nama tindakan "berhasil masuk" di jejak audit,
// kelompok audit.CategorySession.
const ActionSignedIn = "session.signed_in"

// RecordLogin mencatat bahwa orang berpengenal subject berhasil masuk ke org:
// waktu masuk terakhirnya diperbarui, email dan namanya mengikuti penyedia
// identitas (yang kosong tidak menghapus yang tersimpan), dan kejadiannya
// masuk jejak audit — semuanya di satu transaksi.
//
// Hanya untuk pengguna AKTIF. Orang yang tidak dikenal atau dinonaktifkan
// dijawab "tidak ditemukan", dan produk menolak loginnya.
func (s *Service) RecordLogin(ctx context.Context, org uuid.UUID, subject, email, name string) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	u, err := scanUser(tx.QueryRow(ctx, `
		UPDATE appkit_users
		SET last_login_at = now(), updated_at = now(),
		    email = CASE WHEN $3 <> '' THEN $3 ELSE email END,
		    name = CASE WHEN $4 <> '' THEN $4 ELSE name END
		WHERE organization_id = $1 AND external_subject = $2 AND status = 'active'
		RETURNING `+columns, org, subject, clip(strings.TrimSpace(email), maxEmail), clip(clean(name), maxName)))
	if err != nil {
		return User{}, err
	}
	if err := s.trail.RecordForTx(ctx, tx, org, u.ID, audit.Entry{
		Category: audit.CategorySession, Action: ActionSignedIn,
		Target:  audit.Target{Type: TargetType, ID: u.ID.String()},
		Summary: fmt.Sprintf("%s masuk.", u.label()),
	}); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return u, nil
}

// CountByRole mengembalikan jumlah pengguna org per key role — aktif maupun
// nonaktif: orang yang dinonaktifkan tetap memegang rolenya dan dapat
// diaktifkan kembali. Dipasang sebagai roles.Options.UserCounts.
func (s *Service) CountByRole(ctx context.Context, org uuid.UUID) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT role_key, count(*)::int FROM appkit_users
		WHERE organization_id = $1 GROUP BY role_key`, org)
	if err != nil {
		return nil, fmt.Errorf("users: menghitung pemegang role: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			key string
			n   int
		)
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("users: menghitung pemegang role: %w", err)
		}
		out[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("users: menghitung pemegang role: %w", err)
	}
	return out, nil
}

// LockGrants mengambil, di transaksi tx, kunci pemberian akses org: selama tx
// berjalan tidak ada akses yang diberikan atau diubah di organization itu.
// Dipasang sebagai roles.Options.LockAssignments, sehingga menghapus role dan
// memberikannya tidak pernah berselang.
//
// Kuncinya tingkat TRANSAKSI: kunci tingkat sesi tidak menjaga apa pun di
// belakang PgBouncer `pool_mode=transaction`.
func (s *Service) LockGrants(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "appkit_users:"+org.String()); err != nil {
		return fmt.Errorf("users: mengunci pemberian akses: %w", err)
	}
	return nil
}

// Batas isi, sama dengan constraint kolomnya.
const (
	maxSubject = 255
	maxEmail   = 320
	maxName    = 200
)

// clean merapikan nama: spasi berlebih dan karakter kendali dibuang.
func clean(name string) string {
	return strings.Join(strings.FieldsFunc(name, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// clip memotong s sampai max karakter. Untuk isi dari penyedia identitas,
// yang tidak boleh menggagalkan login hanya karena terlalu panjang.
func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// Actor adalah pelaku sebuah perubahan akses.
type Actor struct {
	// UserID adalah pengguna yang melakukannya; uuid.Nil untuk operator dan
	// pemilik saat masuk pertama: tidak ada pengguna di baliknya.
	UserID uuid.UUID
	// Source wajib: SourceScreen, SourceOperator, atau SourceOwner. Riwayat
	// akses tanpa asal-usul tidak menjawab pertanyaan yang membuatnya ada.
	Source string
}

func (a Actor) valid() bool {
	switch a.Source {
	case SourceScreen, SourceOperator, SourceOwner:
		return true
	}
	return false
}

// GrantInput adalah orang yang diberi akses.
type GrantInput struct {
	// Subject adalah pengenal orang itu di penyedia identitas — bukan email.
	Subject string
	Email   string
	Name    string
	// Role adalah roles.Role.Key.
	Role  string
	Actor Actor
}

// Nama tindakan di jejak audit, kelompok audit.CategoryAccess. Target-nya
// {Type: TargetType, ID: User.ID}; Details memuat "source", serta "role" atau
// "role_before" dan "role_after".
const (
	ActionGranted     = "user.access_granted"
	ActionRoleChanged = "user.role_changed"
	ActionSuspended   = "user.suspended"
	ActionReactivated = "user.reactivated"

	TargetType = "user"
)

func (s *Service) record(ctx context.Context, tx pgx.Tx, org uuid.UUID, a Actor, action string, u User, summary string, details map[string]any) error {
	details["source"] = a.Source
	return s.trail.RecordForTx(ctx, tx, org, a.UserID, audit.Entry{
		Category: audit.CategoryAccess, Action: action,
		Target:  audit.Target{Type: TargetType, ID: u.ID.String()},
		Summary: summary, Details: details,
	})
}

// roleName adalah nama role key untuk kalimat jejak audit; key-nya sendiri
// bila rolenya sudah tidak ada.
func (s *Service) roleName(ctx context.Context, org uuid.UUID, key string) string {
	if r, err := s.access.Get(ctx, org, key); err == nil {
		return r.Name
	}
	return key
}

// seats menolak pengguna aktif baru bila batasnya penuh. Dipanggil di dalam
// kunci pemberian akses, sehingga dua pemberian bersamaan tidak sama-sama
// lolos.
func (s *Service) seats(ctx context.Context, db querier, org uuid.UUID) error {
	if s.opts.Seats == nil {
		return nil
	}
	limit, err := s.opts.Seats(ctx, org)
	if err != nil {
		return fmt.Errorf("users: membaca batas pengguna: %w", err)
	}
	if limit < 0 {
		return nil
	}
	var active int64
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM appkit_users WHERE organization_id = $1 AND status = 'active'`, org).Scan(&active); err != nil {
		return fmt.Errorf("users: menghitung pengguna aktif: %w", err)
	}
	if active >= limit {
		return appkit.QuotaExceeded(appkit.LimitUsers, fmt.Sprintf(
			"Kuota pengguna paket sudah penuh: %d dari %d pengguna aktif. Nonaktifkan pengguna yang tidak lagi bekerja, atau naikkan paket.",
			active, limit))
	}
	return nil
}

var errRole = appkit.Validation("Isian belum sesuai.", appkit.FieldError{Field: "role", Message: "Pilih role yang tersedia."})

// ErrBootstrapped: Grant dengan SourceOwner ditolak karena organization itu
// sudah pernah memberi akses kepada seseorang — aktif maupun nonaktif.
// Pemilik yang dinonaktifkan tidak mendapat aksesnya kembali hanya dengan
// masuk lagi; aksesnya dipulihkan administrator atau operator.
var ErrBootstrapped = errors.New("users: organization ini sudah punya pengguna; pemilik tidak diberi akses otomatis")

// Grant memberi — atau memperbarui — akses satu orang di org, TANPA sesi dan
// tanpa memeriksa izin: untuk perintah operator, pemilik saat masuk pertama,
// dan alur produk sendiri (mis. memberi akses orang luar setelah mengikatnya
// ke pelanggannya). org ditentukan kode server.
//
// Orang yang sudah aktif hanya diperbarui dan tidak dihitung ulang terhadap
// batas pengguna. Orang yang dinonaktifkan diaktifkan kembali.
func (s *Service) Grant(ctx context.Context, org uuid.UUID, in GrantInput) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := s.GrantTx(ctx, tx, org, in)
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return u, nil
}

// GrantTx sama dengan Grant, di dalam transaksi tx milik pemanggil: untuk alur
// produk yang harus atomik bersama pemberian aksesnya, misalnya mengikat orang
// luar ke pelanggannya. Kunci pemberian akses diambil di tx dan baru lepas
// saat tx selesai; pemanggil yang sudah memegangnya lewat LockGrants di tx
// yang sama tidak menunggu dirinya sendiri.
func (s *Service) GrantTx(ctx context.Context, tx pgx.Tx, org uuid.UUID, in GrantInput) (User, error) {
	in.Subject = strings.TrimSpace(in.Subject)
	in.Email = strings.TrimSpace(in.Email)
	in.Name = clean(in.Name)
	switch {
	case org == uuid.Nil:
		return User{}, errors.New("users: organization kosong")
	case tx == nil:
		return User{}, errors.New("users: transaksi wajib diisi")
	case !in.Actor.valid():
		return User{}, errors.New("users: pemberian akses wajib menyebut jalannya (Actor.Source)")
	case in.Subject == "" || strings.ContainsFunc(in.Subject, unicode.IsSpace) || utf8.RuneCountInString(in.Subject) > maxSubject:
		return User{}, appkit.Validation("Pengenal orangnya wajib diisi, tanpa spasi.",
			appkit.FieldError{Field: "subject", Message: "Isi pengenal akun orang itu, bukan emailnya."})
	case utf8.RuneCountInString(in.Email) > maxEmail:
		return User{}, appkit.Validation("Isian belum sesuai.", appkit.FieldError{Field: "email", Message: "Alamat email terlalu panjang."})
	case utf8.RuneCountInString(in.Name) > maxName:
		return User{}, appkit.Validation("Isian belum sesuai.", appkit.FieldError{Field: "name", Message: fmt.Sprintf("Nama maksimal %d karakter.", maxName)})
	}

	if err := s.LockGrants(ctx, tx, org); err != nil {
		return User{}, err
	}
	// Pemilik hanya diberi akses otomatis selama organization belum pernah
	// memberi akses kepada siapa pun. Dihitung di dalam kunci: dua login
	// pertama yang bersamaan tidak sama-sama lolos.
	if in.Actor.Source == SourceOwner {
		var any bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM appkit_users WHERE organization_id = $1)`, org).Scan(&any); err != nil {
			return User{}, fmt.Errorf("users: memeriksa pengguna: %w", err)
		}
		if any {
			return User{}, ErrBootstrapped
		}
	}
	// Role dibaca SESUDAH kunci diambil: penghapusan role mengambil kunci yang
	// sama, jadi role yang terbaca di sini tidak dapat terhapus di sela.
	role, err := s.access.Get(ctx, org, in.Role)
	if err != nil {
		if kind(err) == appkit.KindNotFound {
			return User{}, errRole
		}
		return User{}, err
	}

	prev, err := scanUser(tx.QueryRow(ctx, selectUser+` WHERE organization_id = $1 AND external_subject = $2 FOR UPDATE`, org, in.Subject))
	exists := err == nil
	if err != nil && kind(err) != appkit.KindNotFound {
		return User{}, err
	}
	if !exists || prev.Status != StatusActive {
		if err := s.seats(ctx, tx, org); err != nil {
			return User{}, err
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return User{}, err
	}
	// Email dan nama yang kosong tidak menghapus yang tersimpan.
	u, err := scanUser(tx.QueryRow(ctx, `
		INSERT INTO appkit_users (id, organization_id, external_subject, email, name, role_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (organization_id, external_subject) DO UPDATE
		SET email = CASE WHEN EXCLUDED.email <> '' THEN EXCLUDED.email ELSE appkit_users.email END,
		    name = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE appkit_users.name END,
		    role_key = EXCLUDED.role_key, status = 'active', updated_at = now()
		RETURNING `+columns, id, org, in.Subject, in.Email, in.Name, role.Key))
	if err != nil {
		return User{}, err
	}

	switch {
	case !exists:
		err = s.record(ctx, tx, org, in.Actor, ActionGranted, u,
			fmt.Sprintf("Akses diberikan kepada %s sebagai %s.", u.label(), role.Name),
			map[string]any{"role": role.Key})
	case prev.Status != StatusActive:
		err = s.record(ctx, tx, org, in.Actor, ActionReactivated, u,
			fmt.Sprintf("Akses %s diaktifkan kembali sebagai %s.", u.label(), role.Name),
			map[string]any{"role_before": prev.Role, "role_after": role.Key})
	case prev.Role != role.Key:
		err = s.record(ctx, tx, org, in.Actor, ActionRoleChanged, u,
			fmt.Sprintf("Role %s diubah menjadi %s.", u.label(), role.Name),
			map[string]any{"role_before": prev.Role, "role_after": role.Key})
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// Suspend mencabut akses orang berpengenal subject di org beserta seluruh
// sesinya yang berjalan, TANPA sesi dan tanpa memeriksa izin: untuk perintah
// operator. Orang yang sudah nonaktif bukan galat.
func (s *Service) Suspend(ctx context.Context, org uuid.UUID, subject string, a Actor) error {
	if !a.valid() {
		return errors.New("users: pencabutan akses wajib menyebut jalannya (Actor.Source)")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.LockGrants(ctx, tx, org); err != nil {
		return err
	}
	u, err := scanUser(tx.QueryRow(ctx, selectUser+` WHERE organization_id = $1 AND external_subject = $2 FOR UPDATE`, org, strings.TrimSpace(subject)))
	if err != nil {
		return err
	}
	if u.Status == StatusSuspended {
		return nil
	}
	if err := s.suspend(ctx, tx, org, u, a); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) suspend(ctx context.Context, tx pgx.Tx, org uuid.UUID, u User, a Actor) error {
	if err := setStatus(ctx, tx, org, u.ID, StatusSuspended); err != nil {
		return err
	}
	if err := s.opts.RevokeSessions(ctx, tx, org, u.ID); err != nil {
		return fmt.Errorf("users: mencabut sesi: %w", err)
	}
	return s.record(ctx, tx, org, a, ActionSuspended, u,
		fmt.Sprintf("Akses %s dinonaktifkan.", u.label()), map[string]any{"role": u.Role})
}

func setStatus(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, status string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE appkit_users SET status = $3, updated_at = now()
		WHERE organization_id = $1 AND id = $2`, org, id, status); err != nil {
		return fmt.Errorf("users: mengubah status pengguna: %w", err)
	}
	return nil
}

func setRole(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, role string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE appkit_users SET role_key = $3, updated_at = now()
		WHERE organization_id = $1 AND id = $2`, org, id, role); err != nil {
		return fmt.Errorf("users: mengubah role pengguna: %w", err)
	}
	return nil
}

func kind(err error) appkit.Kind {
	if e, ok := errors.AsType[*appkit.Error](err); ok {
		return e.Kind
	}
	return ""
}
