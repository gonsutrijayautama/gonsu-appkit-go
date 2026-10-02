// Package roles mengelola role sebuah organization: kumpulan izin bernama
// yang dipegang penggunanya.
//
// Pembagian tugasnya:
//
//   - IZIN ada di kode. Produk menyerahkan katalognya (Options.Permissions).
//     Sebuah izin hanya berarti bila ada kode yang memeriksanya, jadi izin
//     tidak pernah dibuat dari layar;
//   - ROLE BAWAAN ada di kode (Options.Builtins), tidak punya baris di
//     database, dan tidak dapat diubah atau dihapus. Tepat satu di antaranya
//     administrator: ia memegang seluruh izin internal, termasuk yang
//     ditambahkan rilis berikutnya;
//   - ROLE BUATAN disusun pemegang izin Manage dan disimpan di sini, per
//     organization;
//   - SIAPA memegang role apa tidak disimpan di sini. Itu kolom di tabel
//     pengguna milik produk, yang menyimpan Role.Key. Karena itu "minimal satu
//     administrator aktif" dan kuota pengguna tetap ditegakkan produk.
//
// Pagar role buatan, ditegakkan saat menyimpan DAN saat dibaca:
//
//   - izin bertanda Sensitive hanya dipegang administrator bawaan;
//   - satu role satu audiens: role internal hanya berisi izin internal, role
//     orang luar hanya izin orang luar. Audiens tidak berubah setelah dibuat.
//     Izin beraudiens mesin tidak dipegang role mana pun;
//   - izin baru di katalog tidak pernah masuk sendiri ke role buatan, dan izin
//     yang sudah tidak ada di katalog diabaikan;
//   - role buatan adalah fitur paket (Options.CustomEnabled). Di organization
//     yang paketnya tidak menyertakannya, role buatan tidak memberi izin apa
//     pun; role bawaan tetap berjalan;
//   - jumlahnya dibatasi (Options.MaxCustom), dan role yang masih dipegang
//     pengguna tidak dapat dihapus;
//   - setiap perubahan dicatat beserta pelakunya (Events).
//
// Produk memakai PermissionsOf atau Can di Hooks.Authorize-nya; lihat README.
package roles

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// Manage adalah izin membuat, mengubah, dan menghapus role buatan. Wajib ada
// di katalog dan bertanda Sensitive: yang boleh menyusun role tidak boleh
// dapat memberikan kemampuan itu ke role susunannya.
const Manage appkit.Permission = "settings.roles.manage"

// Audience menyebut untuk siapa sebuah izin atau role.
type Audience string

const (
	// AudienceInternal: orang di dalam organization — staf.
	AudienceInternal Audience = "internal"
	// AudienceExternal: orang di luar organization yang diberi akses terbatas,
	// misalnya pelanggannya. Kode yang memeriksa izin audiens ini WAJIB
	// membatasi datanya ke orang itu, dari sesi, tidak pernah dari body.
	AudienceExternal Audience = "external"
	// AudienceMachine: program di luar aplikasi yang masuk dengan token, bukan
	// orang. Hanya audiens IZIN: tidak ada role beraudiens ini, dan tidak ada
	// role yang memegang izinnya. Tempatnya disiapkan untuk token API, yang
	// kelak mencentang izin dari katalog yang sama.
	AudienceMachine Audience = "machine"
)

// valid: audiens yang sah untuk sebuah izin.
func (a Audience) valid() bool { return a.forRole() || a == AudienceMachine }

// forRole: audiens yang sah untuk sebuah role.
func (a Audience) forRole() bool { return a == AudienceInternal || a == AudienceExternal }

// DefaultMaxCustom adalah batas bawaan jumlah role buatan satu organization.
const DefaultMaxCustom = 30

// Definition adalah satu izin di katalog produk.
type Definition struct {
	Name appkit.Permission `json:"name"`
	// Group dan Label tampil di layar penyusun role; Group mengelompokkan
	// izin, mis. per modul. Label wajib.
	Group string `json:"group"`
	Label string `json:"label"`
	// Audience kosong berarti AudienceInternal.
	Audience Audience `json:"audience"`
	// Sensitive: hanya dipegang administrator bawaan, tidak dapat dicentang ke
	// role lain, dan — karena wajib beraudiens internal — tidak pernah ke
	// token. Untuk izin yang dapat memperluas akses pemegangnya sendiri atau
	// menyentuh tagihan: mengelola pengguna, role, dan langganan.
	Sensitive bool `json:"sensitive"`
}

// Builtin adalah satu role bawaan.
type Builtin struct {
	// Key adalah nilai yang disimpan produk di tabel penggunanya: huruf kecil,
	// angka, dan garis bawah. Tidak diubah setelah dirilis.
	Key         string
	Name        string
	Description string
	// Audience kosong berarti AudienceInternal. AudienceMachine tidak sah.
	Audience Audience
	// Administrator: memegang SELURUH izin internal di katalog. Tepat satu role
	// bawaan bertanda ini, beraudiens internal, dengan Permissions kosong.
	Administrator bool
	// Permissions role yang bukan administrator: izin di katalog, seaudiens,
	// dan tidak Sensitive.
	Permissions []appkit.Permission
}

// Options mengatur Service. Permissions, Builtins, CustomEnabled, dan
// UserCounts wajib.
type Options struct {
	// Permissions adalah katalog izin produk, dalam urutan tampilnya.
	Permissions []Definition
	// Builtins adalah role bawaan, dalam urutan tampilnya.
	Builtins []Builtin
	// CustomEnabled menjawab apakah paket org menyertakan role buatan —
	// biasanya nilai hak pakai paketnya. Wajib, supaya fitur paket tidak
	// terbuka karena lupa dipasang; produk yang tidak menjualnya terpisah
	// mengembalikan true.
	//
	// false: role buatan tidak dapat dibuat atau diubah dan tidak memberi izin
	// apa pun. Menghapusnya tetap boleh.
	CustomEnabled func(ctx context.Context, org uuid.UUID) (bool, error)
	// MaxCustom adalah batas jumlah role buatan satu organization: pengaman,
	// bukan batas yang dijual. Kosong: DefaultMaxCustom.
	MaxCustom int
	// UserCounts mengembalikan jumlah pengguna org per Role.Key. Role yang
	// tidak dipegang siapa pun boleh tidak ada di jawabannya. Dipakai untuk
	// menolak penghapusan role yang masih dipegang pengguna, dan untuk
	// ditampilkan.
	UserCounts func(ctx context.Context, org uuid.UUID) (map[string]int, error)
}

// Service mengelola role.
type Service struct {
	pool  *pgxpool.Pool
	hooks appkit.Hooks
	opts  Options

	// catalog menurut urutan Options.Permissions; defs indeksnya.
	catalog []Definition
	defs    map[appkit.Permission]Definition
	// builtins menurut urutan Options.Builtins; byKey indeksnya.
	builtins []Role
	byKey    map[string]int
}

// New mengembalikan service role. Katalog dan role bawaan diperiksa di sini:
// susunan yang melanggar pagar gagal saat start, bukan saat dipakai.
func New(pool *pgxpool.Pool, hooks appkit.Hooks, opts Options) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("roles: pool wajib diisi")
	case opts.CustomEnabled == nil:
		return nil, errors.New("roles: Options.CustomEnabled wajib diisi")
	case opts.UserCounts == nil:
		return nil, errors.New("roles: Options.UserCounts wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	if hooks.User == nil {
		return nil, errors.New("roles: Hooks.User wajib diisi")
	}
	if opts.MaxCustom <= 0 {
		opts.MaxCustom = DefaultMaxCustom
	}
	s := &Service{pool: pool, hooks: hooks, opts: opts}
	if err := s.loadCatalog(opts.Permissions); err != nil {
		return nil, err
	}
	if err := s.loadBuiltins(opts.Builtins); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) loadCatalog(defs []Definition) error {
	if len(defs) == 0 {
		return errors.New("roles: Options.Permissions wajib diisi")
	}
	s.defs = make(map[appkit.Permission]Definition, len(defs))
	for _, d := range defs {
		d.Group, d.Label = strings.TrimSpace(d.Group), strings.TrimSpace(d.Label)
		if d.Audience == "" {
			d.Audience = AudienceInternal
		}
		switch {
		case d.Name == "" || strings.ContainsFunc(string(d.Name), unicode.IsSpace):
			return fmt.Errorf("roles: nama izin %q tidak sah", d.Name)
		case d.Label == "":
			return fmt.Errorf("roles: izin %s tanpa Label", d.Name)
		case !d.Audience.valid():
			return fmt.Errorf("roles: audiens izin %s tidak dikenal: %q", d.Name, d.Audience)
		case d.Sensitive && d.Audience != AudienceInternal:
			// Izin sensitif hanya dipegang administrator, dan ia role internal.
			// Karena itu pula izin sensitif tidak pernah sampai ke token.
			return fmt.Errorf("roles: izin sensitif %s harus beraudiens internal", d.Name)
		}
		if _, dup := s.defs[d.Name]; dup {
			return fmt.Errorf("roles: izin %s terdaftar dua kali", d.Name)
		}
		s.defs[d.Name] = d
		s.catalog = append(s.catalog, d)
	}
	if d, ok := s.defs[Manage]; !ok || !d.Sensitive {
		return fmt.Errorf("roles: izin %s wajib ada di Options.Permissions dan bertanda Sensitive", Manage)
	}
	return nil
}

// reKey sengaja tanpa tanda hubung: Key role bawaan tidak pernah berbentuk
// UUID, yaitu Key role buatan.
var reKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

func (s *Service) loadBuiltins(defs []Builtin) error {
	s.byKey = make(map[string]int, len(defs))
	administrators := 0
	for _, b := range defs {
		r := Role{
			Key: b.Key, Name: strings.TrimSpace(b.Name), Description: strings.TrimSpace(b.Description),
			Audience: b.Audience, Builtin: true, Active: true,
		}
		if r.Audience == "" {
			r.Audience = AudienceInternal
		}
		switch {
		case !reKey.MatchString(r.Key):
			return fmt.Errorf("roles: Key role bawaan %q tidak sah", r.Key)
		case r.Name == "":
			return fmt.Errorf("roles: role bawaan %s tanpa Name", r.Key)
		case !r.Audience.forRole():
			return fmt.Errorf("roles: audiens role bawaan %s tidak sah: %q", r.Key, r.Audience)
		case s.builtinNamed(r.Name):
			return fmt.Errorf("roles: nama role bawaan %q dipakai dua kali", r.Name)
		}
		if _, dup := s.byKey[r.Key]; dup {
			return fmt.Errorf("roles: role bawaan %s terdaftar dua kali", r.Key)
		}
		if b.Administrator {
			administrators++
			if r.Audience != AudienceInternal || len(b.Permissions) > 0 {
				return fmt.Errorf("roles: role administrator %s harus beraudiens internal dengan Permissions kosong", r.Key)
			}
			r.Permissions = []appkit.Permission{}
			for _, d := range s.catalog {
				if d.Audience == AudienceInternal {
					r.Permissions = append(r.Permissions, d.Name)
				}
			}
		} else {
			perms, problem := s.check(b.Permissions, r.Audience)
			if problem != "" {
				return fmt.Errorf("roles: role bawaan %s: %s", r.Key, problem)
			}
			r.Permissions = perms
		}
		s.byKey[r.Key] = len(s.builtins)
		s.builtins = append(s.builtins, r)
	}
	if administrators != 1 {
		return errors.New("roles: Options.Builtins harus memuat tepat satu role Administrator")
	}
	return nil
}

func (s *Service) builtinNamed(name string) bool {
	return slices.ContainsFunc(s.builtins, func(r Role) bool { return strings.EqualFold(r.Name, name) })
}

// holdable melaporkan apakah izin d boleh dipegang role beraudiens a yang
// bukan administrator.
func holdable(d Definition, a Audience) bool { return !d.Sensitive && d.Audience == a }

// check memeriksa izin yang DIMINTA untuk role beraudiens a. Hasilnya tanpa
// kembaran, menurut urutan katalog; problem menyebut izin pertama yang
// melanggar pagar.
func (s *Service) check(perms []appkit.Permission, a Audience) (out []appkit.Permission, problem string) {
	for _, p := range perms {
		d, ok := s.defs[p]
		switch {
		case !ok:
			return nil, fmt.Sprintf("Izin %q tidak dikenal.", p)
		case d.Sensitive:
			return nil, fmt.Sprintf("Izin “%s” hanya dapat dipegang role administrator bawaan.", d.Label)
		case d.Audience == AudienceMachine:
			return nil, fmt.Sprintf("Izin “%s” tidak dapat diberikan ke role.", d.Label)
		case d.Audience != a && a == AudienceExternal:
			return nil, fmt.Sprintf("Izin “%s” tidak dapat diberikan ke role untuk orang luar.", d.Label)
		case d.Audience != a:
			return nil, fmt.Sprintf("Izin “%s” hanya untuk role orang luar.", d.Label)
		}
	}
	out = []appkit.Permission{}
	for _, d := range s.catalog {
		if slices.Contains(perms, d.Name) {
			out = append(out, d.Name)
		}
	}
	return out, ""
}

// effective menyaring izin yang TERSIMPAN: yang sudah tidak ada di katalog,
// yang kini Sensitive, dan yang audiensnya kini berbeda diabaikan. Katalog
// berubah bersama rilis, sedangkan baris role tidak.
func (s *Service) effective(stored []string, a Audience) []appkit.Permission {
	out := []appkit.Permission{}
	for _, d := range s.catalog {
		if holdable(d, a) && slices.Contains(stored, string(d.Name)) {
			out = append(out, d.Name)
		}
	}
	return out
}

// Role adalah satu role di API.
type Role struct {
	// Key adalah yang disimpan produk di tabel penggunanya: Builtin.Key untuk
	// role bawaan, UUID untuk role buatan. Tidak berubah saat role diganti
	// namanya.
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Audience    Audience `json:"audience"`
	Builtin     bool     `json:"builtin"`
	// Active false: role buatan di organization yang paketnya tidak
	// menyertakan role buatan. Izinnya tetap tampil, tetapi tidak berlaku
	// (PermissionsOf kosong), dan role ini tidak layak diberikan ke pengguna.
	Active      bool                `json:"active"`
	Permissions []appkit.Permission `json:"permissions"`
	// Version dikirim balik saat menyimpan (Input.Version). 0 untuk role
	// bawaan.
	Version int `json:"version"`
	// UpdatedAt null untuk role bawaan.
	UpdatedAt *time.Time `json:"updated_at"`
}

// stored adalah satu baris appkit_roles.
type stored struct {
	id          uuid.UUID
	name        string
	description string
	audience    Audience
	permissions []string
	version     int
	updatedAt   time.Time
}

const selectRole = `
	SELECT id, name, description, audience, permissions, version, updated_at
	FROM appkit_roles`

func scanRole(row pgx.Row) (stored, error) {
	var (
		r        stored
		audience string
	)
	err := row.Scan(&r.id, &r.name, &r.description, &audience, &r.permissions, &r.version, &r.updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return stored{}, errNotFound
	}
	if err != nil {
		return stored{}, fmt.Errorf("roles: membaca role: %w", err)
	}
	r.audience = Audience(audience)
	return r, nil
}

func (s *Service) role(r stored, active bool) Role {
	return Role{
		Key: r.id.String(), Name: r.name, Description: r.description, Audience: r.audience,
		Active: active, Permissions: s.effective(r.permissions, r.audience),
		Version: r.version, UpdatedAt: &r.updatedAt,
	}
}

func (r stored) snapshot() *Snapshot {
	perms := make([]appkit.Permission, len(r.permissions))
	for i, p := range r.permissions {
		perms[i] = appkit.Permission(p)
	}
	return &Snapshot{Name: r.name, Description: r.description, Audience: r.audience, Permissions: perms}
}

var (
	errNotFound = appkit.NotFound("Role tidak ditemukan.")
	errBuiltin  = appkit.Validation("Role bawaan tidak dapat diubah atau dihapus.")
)

// builtin mengembalikan salinan role bawaan key: pemanggil tidak boleh dapat
// mengubah daftar izin yang dipakai bersama.
func (s *Service) builtin(key string) (Role, bool) {
	i, ok := s.byKey[key]
	if !ok {
		return Role{}, false
	}
	r := s.builtins[i]
	r.Permissions = slices.Clone(r.Permissions)
	return r, true
}

// customID mengurai Key role buatan. Hanya bentuk UUID baku yang diterima:
// Key adalah nilai yang disimpan produk, dan dua tulisan untuk role yang sama
// membuat hitungan pemegangnya meleset.
func customID(key string) (uuid.UUID, bool) {
	id, err := uuid.Parse(key)
	return id, err == nil && id.String() == key
}

func (s *Service) enabled(ctx context.Context, org uuid.UUID) (bool, error) {
	ok, err := s.opts.CustomEnabled(ctx, org)
	if err != nil {
		return false, fmt.Errorf("roles: membaca hak pakai role buatan: %w", err)
	}
	return ok, nil
}

// custom membaca role buatan org, urut nama.
func (s *Service) custom(ctx context.Context, org uuid.UUID, active bool) ([]Role, error) {
	rows, err := s.pool.Query(ctx, selectRole+` WHERE organization_id = $1 ORDER BY lower(name), id`, org)
	if err != nil {
		return nil, fmt.Errorf("roles: membaca role: %w", err)
	}
	defer rows.Close()
	out := []Role{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s.role(r, active))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("roles: membaca role: %w", err)
	}
	return out, nil
}

// Get membaca role key milik org, TANPA memeriksa izin: untuk produk, saat
// memberikan role ke pengguna. org adalah organization sesi yang sudah
// diperiksa produk. Role buatan milik organization lain dijawab "tidak
// ditemukan".
//
// Sebelum menyimpan Key ke penggunanya, produk memeriksa Active dan
// mencocokkan Audience dengan jenis pengguna itu: role orang luar tidak untuk
// staf, dan sebaliknya.
func (s *Service) Get(ctx context.Context, org uuid.UUID, key string) (Role, error) {
	if r, ok := s.builtin(key); ok {
		return r, nil
	}
	id, ok := customID(key)
	if !ok {
		return Role{}, errNotFound
	}
	r, err := scanRole(s.pool.QueryRow(ctx, selectRole+` WHERE organization_id = $1 AND id = $2`, org, id))
	if err != nil {
		return Role{}, err
	}
	active, err := s.enabled(ctx, org)
	if err != nil {
		return Role{}, err
	}
	return s.role(r, active), nil
}

// All mengembalikan seluruh role org TANPA memeriksa izin: role bawaan
// menurut urutan Options.Builtins, lalu role buatan urut nama. Untuk produk,
// misalnya mengisi pilihan role di layar pengguna.
func (s *Service) All(ctx context.Context, org uuid.UUID) ([]Role, error) {
	active, err := s.enabled(ctx, org)
	if err != nil {
		return nil, err
	}
	return s.all(ctx, org, active)
}

func (s *Service) all(ctx context.Context, org uuid.UUID, active bool) ([]Role, error) {
	custom, err := s.custom(ctx, org, active)
	if err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(s.builtins)+len(custom))
	for _, r := range s.builtins {
		r.Permissions = slices.Clone(r.Permissions)
		out = append(out, r)
	}
	return append(out, custom...), nil
}

// PermissionsOf mengembalikan izin yang BERLAKU bagi pemegang role key di
// org, untuk dipakai produk di Hooks.Authorize dan di jawaban "siapa saya".
//
// Jawabannya kosong, bukan galat, untuk role yang tidak ada, role milik
// organization lain, dan role buatan di organization yang paketnya tidak
// menyertakan role buatan: yang tidak dikenal tidak memegang izin apa pun.
// Role bawaan dijawab tanpa menyentuh database.
func (s *Service) PermissionsOf(ctx context.Context, org uuid.UUID, key string) ([]appkit.Permission, error) {
	r, err := s.Get(ctx, org, key)
	if errors.Is(err, errNotFound) {
		return []appkit.Permission{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !r.Active {
		return []appkit.Permission{}, nil
	}
	return r.Permissions, nil
}

// Can melaporkan apakah pemegang role key di org memegang perm.
func (s *Service) Can(ctx context.Context, org uuid.UUID, key string, perm appkit.Permission) (bool, error) {
	perms, err := s.PermissionsOf(ctx, org, key)
	if err != nil {
		return false, err
	}
	return slices.Contains(perms, perm), nil
}
