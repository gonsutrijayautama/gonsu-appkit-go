package roles_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/roles"
)

// Izin "produk" di test ini.
const (
	usersManage appkit.Permission = "settings.users.manage"
	notesRead   appkit.Permission = "notes.read"
	notesWrite  appkit.Permission = "notes.write"
	portalView  appkit.Permission = "portal.view"
	portalOrder appkit.Permission = "portal.order"
	apiOrders   appkit.Permission = "api.orders.read"
)

func catalog() []roles.Definition {
	return []roles.Definition{
		{Name: usersManage, Group: "Pengaturan", Label: "Mengelola pengguna", Sensitive: true},
		{Name: roles.Manage, Group: "Pengaturan", Label: "Mengelola role", Sensitive: true},
		{Name: notesRead, Group: "Catatan", Label: "Melihat catatan"},
		{Name: notesWrite, Group: "Catatan", Label: "Mengubah catatan"},
		{Name: portalView, Group: "Portal", Label: "Melihat pesanan sendiri", Audience: roles.AudienceExternal},
		{Name: portalOrder, Group: "Portal", Label: "Memesan sendiri", Audience: roles.AudienceExternal},
		{Name: apiOrders, Group: "API", Label: "Membaca pesanan", Audience: roles.AudienceMachine},
	}
}

func builtins() []roles.Builtin {
	return []roles.Builtin{
		{Key: "administrator", Name: "Administrator", Administrator: true},
		{Key: "staff", Name: "Staf", Permissions: []appkit.Permission{notesWrite, notesRead}},
		{Key: "customer", Name: "Customer", Audience: roles.AudienceExternal, Permissions: []appkit.Permission{portalView}},
	}
}

// fixture memegang yang biasanya dijawab produk: hak pakai role buatan dan
// jumlah pemegang tiap role.
type fixture struct {
	pool  *pgxpool.Pool
	trail *audit.Service
	roles *roles.Service

	mu       sync.Mutex
	disabled map[uuid.UUID]bool
	holders  map[uuid.UUID]map[string]int
	failing  error
}

func (f *fixture) options() roles.Options {
	return roles.Options{
		Permissions: catalog(),
		Builtins:    builtins(),
		CustomEnabled: func(_ context.Context, org uuid.UUID) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return !f.disabled[org], f.failing
		},
		UserCounts: func(_ context.Context, org uuid.UUID) (map[string]int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.holders[org], nil
		},
	}
}

func setup(t *testing.T, change ...func(*roles.Options)) *fixture {
	t.Helper()
	f := &fixture{pool: testdb.New(t), disabled: map[uuid.UUID]bool{}, holders: map[uuid.UUID]map[string]int{}}
	f.trail = testdb.Trail(t, f.pool)
	opts := f.options()
	for _, c := range change {
		c(&opts)
	}
	s, err := roles.New(f.pool, f.trail, testdb.Hooks(), opts)
	if err != nil {
		t.Fatal(err)
	}
	f.roles = s
	return f
}

func (f *fixture) disable(org uuid.UUID, off bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disabled[org] = off
}

func (f *fixture) hold(org uuid.UUID, key string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holders[org] = map[string]int{key: n}
}

// admin memegang izin Manage; staff tidak.
func admin(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{
		Organization: org, User: uuid.New(), Permissions: []appkit.Permission{roles.Manage},
	})
}

func staff(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{Organization: org, User: uuid.New()})
}

func kind(err error) appkit.Kind {
	if e, ok := errors.AsType[*appkit.Error](err); ok {
		return e.Kind
	}
	return ""
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	e, ok := errors.AsType[*appkit.Error](err)
	if !ok || e.Kind != appkit.KindValidation {
		t.Fatalf("galat = %v, ingin galat validasi", err)
	}
	out := map[string]string{}
	for _, f := range e.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func (f *fixture) create(t *testing.T, ctx context.Context, in roles.Input) roles.Role {
	t.Helper()
	r, err := f.roles.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create %q: %v", in.Name, err)
	}
	return r
}

func (f *fixture) permissions(t *testing.T, org uuid.UUID, key string) []appkit.Permission {
	t.Helper()
	perms, err := f.roles.PermissionsOf(context.Background(), org, key)
	if err != nil {
		t.Fatalf("PermissionsOf %s: %v", key, err)
	}
	return perms
}

// Role bawaan ada di kode: berlaku di setiap organization tanpa baris apa pun.
func TestBuiltins(t *testing.T) {
	f := setup(t)
	org := uuid.New()

	// Administrator memegang seluruh izin internal, termasuk yang sensitif,
	// dan tidak satu pun izin orang luar atau izin mesin.
	want := []appkit.Permission{usersManage, roles.Manage, notesRead, notesWrite}
	if got := f.permissions(t, org, "administrator"); !slices.Equal(got, want) {
		t.Errorf("izin administrator = %v, ingin %v", got, want)
	}
	// Urutannya urutan katalog, bukan urutan penulisan di Builtin.
	if got := f.permissions(t, org, "staff"); !slices.Equal(got, []appkit.Permission{notesRead, notesWrite}) {
		t.Errorf("izin staff = %v", got)
	}
	if got := f.permissions(t, org, "customer"); !slices.Equal(got, []appkit.Permission{portalView}) {
		t.Errorf("izin customer = %v", got)
	}

	for perm, want := range map[appkit.Permission]bool{notesRead: true, usersManage: false, portalView: false, apiOrders: false} {
		if got, err := f.roles.Can(context.Background(), org, "staff", perm); err != nil || got != want {
			t.Errorf("Can(staff, %s) = %v, %v; ingin %v", perm, got, err, want)
		}
	}

	// Yang tidak dikenal tidak memegang izin apa pun, dan itu bukan galat.
	for _, key := range []string{"", "pemilik", "ADMINISTRATOR", uuid.NewString()} {
		if got := f.permissions(t, org, key); len(got) != 0 {
			t.Errorf("izin role %q = %v, ingin kosong", key, got)
		}
		if _, err := f.roles.Get(context.Background(), org, key); kind(err) != appkit.KindNotFound {
			t.Errorf("Get(%q) = %v, ingin tidak ditemukan", key, err)
		}
	}

	// Daftar izin role bawaan tidak dapat diubah lewat jawabannya.
	got := f.permissions(t, org, "staff")
	got[0] = usersManage
	if again := f.permissions(t, org, "staff"); again[0] != notesRead {
		t.Errorf("izin role bawaan berubah lewat jawaban PermissionsOf: %v", again)
	}

	l, err := f.roles.List(admin(org))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Roles) != 3 || l.Roles[0].Key != "administrator" || !l.Roles[0].Builtin ||
		l.Roles[0].Version != 0 || l.Roles[0].UpdatedAt != nil || l.Roles[2].Audience != roles.AudienceExternal {
		t.Errorf("role bawaan di daftar = %+v", l.Roles)
	}
	if len(l.Permissions) != len(catalog()) || l.Permissions[2].Audience != roles.AudienceInternal {
		t.Errorf("katalog = %+v", l.Permissions)
	}
	if l.Custom != (roles.Custom{Enabled: true, Count: 0, Max: roles.DefaultMaxCustom}) {
		t.Errorf("custom = %+v", l.Custom)
	}
}

func TestCreate(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)

	r := f.create(t, ctx, roles.Input{
		Name: "  Kasir ", Description: " Hanya catatan ",
		Permissions: []appkit.Permission{notesWrite, notesRead, notesWrite},
	})
	if r.Name != "Kasir" || r.Description != "Hanya catatan" {
		t.Errorf("isian tidak dirapikan: %+v", r)
	}
	// Audiens bawaannya internal; izinnya tanpa kembaran, urut katalog.
	if r.Audience != roles.AudienceInternal || !slices.Equal(r.Permissions, []appkit.Permission{notesRead, notesWrite}) {
		t.Errorf("role = %+v", r)
	}
	if r.Builtin || r.Version != 1 || r.UpdatedAt == nil {
		t.Errorf("role = %+v", r)
	}
	if _, err := uuid.Parse(r.Key); err != nil {
		t.Errorf("Key role buatan = %q, ingin UUID", r.Key)
	}

	if got := f.permissions(t, org, r.Key); !slices.Equal(got, r.Permissions) {
		t.Errorf("PermissionsOf = %v", got)
	}
	if got, err := f.roles.Get(context.Background(), org, r.Key); err != nil || got.Name != "Kasir" {
		t.Errorf("Get = %+v, %v", got, err)
	}
	// Tulisan lain untuk UUID yang sama bukan Key yang sah.
	if got := f.permissions(t, org, strings.ToUpper(r.Key)); len(got) != 0 {
		t.Errorf("Key berhuruf besar memegang izin: %v", got)
	}

	f.hold(org, r.Key, 3)
	l, err := f.roles.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Roles) != 4 || l.Roles[3].Key != r.Key || l.Custom.Count != 1 {
		t.Errorf("daftar = %+v", l)
	}
	if l.Users[r.Key] != 3 || len(l.Users) != 4 {
		t.Errorf("jumlah pemegang = %v", l.Users)
	}
	if _, ok := l.Users["staff"]; !ok {
		t.Errorf("role tanpa pemegang tidak punya isian: %v", l.Users)
	}

	all, err := f.roles.All(context.Background(), org)
	if err != nil || len(all) != 4 || all[3].Key != r.Key {
		t.Errorf("All = %+v, %v", all, err)
	}
}

func TestCreateValidation(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	ok := []appkit.Permission{notesRead}
	f.create(t, ctx, roles.Input{Name: "Kasir", Permissions: ok})

	for name, tc := range map[string]struct {
		in    roles.Input
		field string
	}{
		"nama kosong":             {roles.Input{Name: "  ", Permissions: ok}, "name"},
		"nama terlalu panjang":    {roles.Input{Name: strings.Repeat("a", 61), Permissions: ok}, "name"},
		"nama role bawaan":        {roles.Input{Name: "administrator", Permissions: ok}, "name"},
		"nama sudah dipakai":      {roles.Input{Name: "KASIR", Permissions: ok}, "name"},
		"keterangan panjang":      {roles.Input{Name: "A", Description: strings.Repeat("a", 201), Permissions: ok}, "description"},
		"audiens tak dikenal":     {roles.Input{Name: "A", Audience: "publik", Permissions: ok}, "audience"},
		"tanpa izin":              {roles.Input{Name: "A"}, "permissions"},
		"izin tak dikenal":        {roles.Input{Name: "A", Permissions: []appkit.Permission{"notes.delete"}}, "permissions"},
		"izin sensitif":           {roles.Input{Name: "A", Permissions: []appkit.Permission{notesRead, usersManage}}, "permissions"},
		"izin mengelola role":     {roles.Input{Name: "A", Permissions: []appkit.Permission{roles.Manage}}, "permissions"},
		"izin luar di internal":   {roles.Input{Name: "A", Permissions: []appkit.Permission{notesRead, portalView}}, "permissions"},
		"izin internal di luar":   {roles.Input{Name: "A", Audience: roles.AudienceExternal, Permissions: []appkit.Permission{portalView, notesRead}}, "permissions"},
		"izin sensitif di luar":   {roles.Input{Name: "A", Audience: roles.AudienceExternal, Permissions: []appkit.Permission{usersManage}}, "permissions"},
		"izin mesin di internal":  {roles.Input{Name: "A", Permissions: []appkit.Permission{notesRead, apiOrders}}, "permissions"},
		"izin mesin di luar":      {roles.Input{Name: "A", Audience: roles.AudienceExternal, Permissions: []appkit.Permission{apiOrders}}, "permissions"},
		"role beraudiens mesin":   {roles.Input{Name: "A", Audience: roles.AudienceMachine, Permissions: []appkit.Permission{apiOrders}}, "audience"},
		"version negatif":         {roles.Input{Name: "A", Permissions: ok, Version: -1}, "version"},
		"nama dan izin sekaligus": {roles.Input{Permissions: []appkit.Permission{usersManage}}, "permissions"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.roles.Create(ctx, tc.in)
			if fields := fieldErrors(t, err); fields[tc.field] == "" {
				t.Errorf("tidak ada galat untuk %s: %v", tc.field, fields)
			}
		})
	}

	// Yang gagal validasi tidak menyimpan apa pun, termasuk catatannya.
	if l, _ := f.roles.List(ctx); l.Custom.Count != 1 {
		t.Errorf("isian yang tidak sah tersimpan: %+v", l.Roles)
	}
	if actions := testdb.Actions(t, f.trail, org); len(actions) != 1 {
		t.Errorf("isian yang tidak sah tercatat: %v", actions)
	}
}

// Satu role satu audiens: role untuk orang luar boleh disusun sendiri, tetapi
// hanya dari izin orang luar.
func TestExternalAudience(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)

	r := f.create(t, ctx, roles.Input{
		Name: "Customer Pemesan", Audience: roles.AudienceExternal,
		Permissions: []appkit.Permission{portalOrder, portalView},
	})
	if r.Audience != roles.AudienceExternal || !slices.Equal(r.Permissions, []appkit.Permission{portalView, portalOrder}) {
		t.Errorf("role orang luar = %+v", r)
	}

	// Audiens tidak berubah setelah dibuat; mengosongkannya berarti tetap.
	_, err := f.roles.Update(ctx, r.Key, roles.Input{
		Name: r.Name, Audience: roles.AudienceInternal, Permissions: []appkit.Permission{notesRead}, Version: r.Version,
	})
	if fields := fieldErrors(t, err); fields["audience"] == "" {
		t.Errorf("mengubah audiens = %v", fields)
	}
	if _, err := f.roles.Update(ctx, r.Key, roles.Input{Name: r.Name, Permissions: []appkit.Permission{notesRead}, Version: r.Version}); fieldErrors(t, err)["permissions"] == "" {
		t.Errorf("izin internal masuk role orang luar: %v", err)
	}
	got, err := f.roles.Update(ctx, r.Key, roles.Input{Name: "Customer Pantau", Permissions: []appkit.Permission{portalView}, Version: r.Version})
	if err != nil || got.Audience != roles.AudienceExternal || !slices.Equal(got.Permissions, []appkit.Permission{portalView}) {
		t.Errorf("Update role orang luar = %+v, %v", got, err)
	}
}

// Izin ditegakkan service, dan galat dari pengait diteruskan apa adanya.
func TestOnlyManageMayChange(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	r := f.create(t, admin(org), roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})
	in := roles.Input{Name: "Kasir Dua", Permissions: []appkit.Permission{notesRead}, Version: r.Version}

	for name, ctx := range map[string]context.Context{"tanpa izin": staff(org), "tanpa sesi": context.Background()} {
		want := testdb.ErrDenied
		if name == "tanpa sesi" {
			want = testdb.ErrNoSession
		}
		if _, err := f.roles.List(ctx); !errors.Is(err, want) {
			t.Errorf("List %s = %v", name, err)
		}
		if _, err := f.roles.Create(ctx, in); !errors.Is(err, want) {
			t.Errorf("Create %s = %v", name, err)
		}
		if _, err := f.roles.Update(ctx, r.Key, in); !errors.Is(err, want) {
			t.Errorf("Update %s = %v", name, err)
		}
		if err := f.roles.Delete(ctx, r.Key); !errors.Is(err, want) {
			t.Errorf("Delete %s = %v", name, err)
		}
	}

	// Pemegang izin tanpa identitas pengguna tidak dapat mengubah: perubahan
	// tanpa pelaku tidak pernah tersimpan.
	anonymous := testdb.With(context.Background(), testdb.Session{Organization: org, Permissions: []appkit.Permission{roles.Manage}})
	if _, err := f.roles.Create(anonymous, in); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Create tanpa pengguna = %v", err)
	}
	if _, err := f.roles.Update(anonymous, r.Key, in); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Update tanpa pengguna = %v", err)
	}
	if err := f.roles.Delete(anonymous, r.Key); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Delete tanpa pengguna = %v", err)
	}

	l, err := f.roles.List(admin(org))
	if err != nil || l.Custom.Count != 1 || l.Roles[3].Name != "Kasir" {
		t.Errorf("percobaan tanpa izin mengubah role: %+v, %v", l.Roles, err)
	}
}

func TestTenantIsolation(t *testing.T) {
	f := setup(t)
	a, b := uuid.New(), uuid.New()
	perms := []appkit.Permission{notesRead}
	r := f.create(t, admin(a), roles.Input{Name: "Kasir", Permissions: perms})

	// Role A tidak ada bagi organization B, di jalur mana pun.
	if l, err := f.roles.List(admin(b)); err != nil || l.Custom.Count != 0 || len(l.Roles) != 3 {
		t.Errorf("organization B melihat role A: %+v, %v", l.Roles, err)
	}
	if all, err := f.roles.All(context.Background(), b); err != nil || len(all) != 3 {
		t.Errorf("All organization B = %+v, %v", all, err)
	}
	if _, err := f.roles.Get(context.Background(), b, r.Key); kind(err) != appkit.KindNotFound {
		t.Errorf("Get lintas organization = %v, ingin tidak ditemukan", err)
	}
	if got := f.permissions(t, b, r.Key); len(got) != 0 {
		t.Errorf("role A memberi izin di organization B: %v", got)
	}
	if _, err := f.roles.Update(admin(b), r.Key, roles.Input{Name: "Dibajak", Permissions: perms, Version: r.Version}); kind(err) != appkit.KindNotFound {
		t.Errorf("Update lintas organization = %v, ingin tidak ditemukan", err)
	}
	if err := f.roles.Delete(admin(b), r.Key); kind(err) != appkit.KindNotFound {
		t.Errorf("Delete lintas organization = %v, ingin tidak ditemukan", err)
	}
	if actions := testdb.Actions(t, f.trail, b); len(actions) != 0 {
		t.Errorf("percobaan organization B tercatat: %v", actions)
	}

	// Nama yang sama boleh dipakai organization lain, dan batasnya terpisah.
	f.create(t, admin(b), roles.Input{Name: "Kasir", Permissions: perms})

	if got, err := f.roles.Get(context.Background(), a, r.Key); err != nil || got.Name != "Kasir" || got.Version != 1 {
		t.Errorf("role A berubah oleh organization B: %+v, %v", got, err)
	}
	if actions := testdb.Actions(t, f.trail, a); len(actions) != 1 {
		t.Errorf("catatan A = %v", actions)
	}
}

func TestBuiltinsCannotBeChanged(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)

	for _, key := range []string{"administrator", "staff", "customer"} {
		_, err := f.roles.Update(ctx, key, roles.Input{Name: "Lain", Permissions: []appkit.Permission{notesRead}})
		if kind(err) != appkit.KindValidation {
			t.Errorf("Update role bawaan %s = %v", key, err)
		}
		if err := f.roles.Delete(ctx, key); kind(err) != appkit.KindValidation {
			t.Errorf("Delete role bawaan %s = %v", key, err)
		}
	}
	if actions := testdb.Actions(t, f.trail, org); len(actions) != 0 {
		t.Errorf("percobaan mengubah role bawaan tercatat: %v", actions)
	}
}

func TestUpdate(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	r := f.create(t, ctx, roles.Input{Name: "Kasir", Description: "Lama", Permissions: []appkit.Permission{notesRead}})
	other := f.create(t, ctx, roles.Input{Name: "Gudang", Permissions: []appkit.Permission{notesRead}})

	// Menyimpan mengganti SELURUH isian: keterangan yang tidak dikirim kosong.
	got, err := f.roles.Update(ctx, r.Key, roles.Input{Name: "Kasir Senior", Permissions: []appkit.Permission{notesWrite}, Version: r.Version})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Key != r.Key || got.Name != "Kasir Senior" || got.Description != "" || got.Version != 2 ||
		!slices.Equal(got.Permissions, []appkit.Permission{notesWrite}) {
		t.Errorf("role sesudah Update = %+v", got)
	}
	// Izin yang dicabut langsung tidak berlaku.
	if perms := f.permissions(t, org, r.Key); !slices.Equal(perms, []appkit.Permission{notesWrite}) {
		t.Errorf("PermissionsOf sesudah Update = %v", perms)
	}

	// Dua administrator membuka formulir yang sama: yang menyimpan belakangan
	// ditolak, bukan menimpa diam-diam.
	for _, version := range []int{0, r.Version, 99} {
		_, err := f.roles.Update(ctx, r.Key, roles.Input{Name: "Timpa", Permissions: []appkit.Permission{notesRead}, Version: version})
		if kind(err) != appkit.KindConflict {
			t.Errorf("Update dengan version %d = %v, ingin konflik", version, err)
		}
	}

	// Nama role lain ditolak; namanya sendiri dengan huruf lain tidak.
	_, err = f.roles.Update(ctx, r.Key, roles.Input{Name: "gudang", Permissions: []appkit.Permission{notesRead}, Version: got.Version})
	if fields := fieldErrors(t, err); fields["name"] == "" {
		t.Errorf("nama role lain = %v", fields)
	}
	if _, err := f.roles.Update(ctx, r.Key, roles.Input{Name: "KASIR SENIOR", Permissions: []appkit.Permission{notesRead}, Version: got.Version}); err != nil {
		t.Errorf("mengganti huruf nama sendiri: %v", err)
	}

	if _, err := f.roles.Update(ctx, uuid.NewString(), roles.Input{Name: "A", Permissions: []appkit.Permission{notesRead}}); kind(err) != appkit.KindNotFound {
		t.Errorf("Update role yang tidak ada = %v", err)
	}
	if _, err := f.roles.Update(ctx, "bukan-key", roles.Input{Name: "A", Permissions: []appkit.Permission{notesRead}}); kind(err) != appkit.KindNotFound {
		t.Errorf("Update Key yang salah bentuk = %v", err)
	}
	if got, _ := f.roles.Get(context.Background(), org, other.Key); got.Name != "Gudang" || got.Version != 1 {
		t.Errorf("role lain ikut berubah: %+v", got)
	}
}

func TestDelete(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	r := f.create(t, ctx, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})

	// Role yang masih dipegang pengguna tidak dapat dihapus.
	f.hold(org, r.Key, 2)
	err := f.roles.Delete(ctx, r.Key)
	if kind(err) != appkit.KindValidation || !strings.Contains(err.Error(), "2 pengguna") {
		t.Errorf("Delete role yang dipakai = %v", err)
	}
	if got := f.permissions(t, org, r.Key); len(got) != 1 {
		t.Errorf("role yang gagal dihapus kehilangan izin: %v", got)
	}

	f.hold(org, r.Key, 0)
	if err := f.roles.Delete(ctx, r.Key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Key yang tertinggal di pengguna tidak memegang izin apa pun.
	if got := f.permissions(t, org, r.Key); len(got) != 0 {
		t.Errorf("role terhapus masih memberi izin: %v", got)
	}
	if err := f.roles.Delete(ctx, r.Key); kind(err) != appkit.KindNotFound {
		t.Errorf("Delete kedua = %v, ingin tidak ditemukan", err)
	}
	// Namanya dapat dipakai lagi.
	f.create(t, ctx, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})
}

// Produk boleh memasang foreign key dari tabel penggunanya; penghapusan yang
// ditolak database dijawab sebagai "masih dipakai", bukan galat tak terduga.
func TestDeleteRejectedByProductForeignKey(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	r := f.create(t, ctx, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})

	bg := context.Background()
	if _, err := f.pool.Exec(bg, `
		CREATE TABLE appkit_test_users (
			organization_id uuid NOT NULL,
			role_id         uuid NOT NULL,
			FOREIGN KEY (organization_id, role_id) REFERENCES appkit_roles (organization_id, id)
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(bg, `INSERT INTO appkit_test_users VALUES ($1, $2)`, org, r.Key); err != nil {
		t.Fatal(err)
	}
	// Rujukan lintas organization ditolak database.
	if _, err := f.pool.Exec(bg, `INSERT INTO appkit_test_users VALUES ($1, $2)`, uuid.New(), r.Key); err == nil {
		t.Error("foreign key menerima role milik organization lain")
	}

	if err := f.roles.Delete(ctx, r.Key); kind(err) != appkit.KindValidation {
		t.Errorf("Delete yang ditolak foreign key = %v", err)
	}
	if actions := testdb.Actions(t, f.trail, org); len(actions) != 1 {
		t.Errorf("penghapusan yang gagal tercatat: %v", actions)
	}
}

// Role buatan adalah fitur paket, dan yang dijual adalah kemampuan MENYUSUN.
// Tanpa hak pakai, role buatan tidak dapat dibuat atau diubah, tetapi yang
// sudah ada tetap berlaku: organization yang turun paket tidak boleh
// mendapati stafnya terkunci seketika.
func TestCustomRolesNeedTheEntitlement(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	in := roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}}
	r := f.create(t, ctx, in)

	f.disable(org, true)

	if _, err := f.roles.Create(ctx, roles.Input{Name: "Gudang", Permissions: in.Permissions}); kind(err) != appkit.KindQuotaExceeded {
		t.Errorf("Create tanpa hak pakai = %v", err)
	}
	// Hak pakai diperiksa sebelum isian: yang tidak berhak tidak diberi tahu
	// isiannya salah di mana.
	if _, err := f.roles.Create(ctx, roles.Input{}); kind(err) != appkit.KindQuotaExceeded {
		t.Errorf("Create isian kosong tanpa hak pakai = %v", err)
	}
	in.Version = r.Version
	if _, err := f.roles.Update(ctx, r.Key, in); kind(err) != appkit.KindQuotaExceeded {
		t.Errorf("Update tanpa hak pakai = %v", err)
	}

	// Role buatan yang sudah ada tetap berlaku dan tetap dapat diberikan.
	if got := f.permissions(t, org, r.Key); !slices.Equal(got, []appkit.Permission{notesRead}) {
		t.Errorf("role buatan tanpa hak pakai = %v, ingin tetap berlaku", got)
	}
	if got, err := f.roles.Get(context.Background(), org, r.Key); err != nil || len(got.Permissions) != 1 {
		t.Errorf("Get tanpa hak pakai = %+v, %v", got, err)
	}
	l, err := f.roles.List(ctx)
	if err != nil || l.Custom.Enabled || l.Custom.Count != 1 || len(l.Roles[3].Permissions) != 1 {
		t.Errorf("List tanpa hak pakai = %+v, %v", l, err)
	}

	// Organization lain tidak terpengaruh.
	f.create(t, admin(uuid.New()), roles.Input{Name: "Kasir", Permissions: in.Permissions})

	// Menghapus tetap boleh tanpa hak pakai.
	if err := f.roles.Delete(ctx, r.Key); err != nil {
		t.Errorf("Delete tanpa hak pakai = %v", err)
	}
}

// Hak pakai hanya dibaca saat menyusun role. Saat ia tidak terbaca, menyusun
// gagal, tetapi pemeriksaan izin tidak: hak pakai yang gagal dibaca tidak
// dapat mengunci siapa pun.
func TestEntitlementFailure(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	r := f.create(t, ctx, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})

	boom := errors.New("lease tidak terbaca")
	f.mu.Lock()
	f.failing = boom
	f.mu.Unlock()

	if _, err := f.roles.Create(ctx, roles.Input{Name: "Gudang", Permissions: []appkit.Permission{notesRead}}); !errors.Is(err, boom) {
		t.Errorf("Create = %v", err)
	}
	if _, err := f.roles.Update(ctx, r.Key, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}, Version: r.Version}); !errors.Is(err, boom) {
		t.Errorf("Update = %v", err)
	}
	if _, err := f.roles.List(ctx); !errors.Is(err, boom) {
		t.Errorf("List = %v", err)
	}

	if got := f.permissions(t, org, r.Key); !slices.Equal(got, []appkit.Permission{notesRead}) {
		t.Errorf("PermissionsOf role buatan saat hak pakai tidak terbaca = %v", got)
	}
	if ok, err := f.roles.Can(context.Background(), org, r.Key, notesRead); !ok || err != nil {
		t.Errorf("Can = %v, %v", ok, err)
	}
	if got := f.permissions(t, org, "administrator"); len(got) != 4 {
		t.Errorf("role bawaan saat hak pakai tidak terbaca = %v", got)
	}
	if all, err := f.roles.All(context.Background(), org); err != nil || len(all) != 4 {
		t.Errorf("All = %+v, %v", all, err)
	}
	if err := f.roles.Delete(ctx, r.Key); err != nil {
		t.Errorf("Delete = %v", err)
	}
}

func TestLimit(t *testing.T) {
	f := setup(t, func(o *roles.Options) { o.MaxCustom = 2 })
	org := uuid.New()
	ctx := admin(org)
	perms := []appkit.Permission{notesRead}

	first := f.create(t, ctx, roles.Input{Name: "Satu", Permissions: perms})
	f.create(t, ctx, roles.Input{Name: "Dua", Permissions: perms})
	_, err := f.roles.Create(ctx, roles.Input{Name: "Tiga", Permissions: perms})
	if kind(err) != appkit.KindValidation || !strings.Contains(err.Error(), "batas (2)") {
		t.Errorf("Create di atas batas = %v", err)
	}
	if l, _ := f.roles.List(ctx); l.Custom != (roles.Custom{Enabled: true, Count: 2, Max: 2}) {
		t.Errorf("custom = %+v", l.Custom)
	}

	// Batasnya per organization, dan menghapus membuka tempat.
	f.create(t, admin(uuid.New()), roles.Input{Name: "Satu", Permissions: perms})
	if err := f.roles.Delete(ctx, first.Key); err != nil {
		t.Fatal(err)
	}
	f.create(t, ctx, roles.Input{Name: "Tiga", Permissions: perms})
}

// Pembuatan bersamaan tidak melewati batas: batasnya keras, bukan lunak.
func TestLimitHoldsUnderConcurrency(t *testing.T) {
	f := setup(t, func(o *roles.Options) { o.MaxCustom = 3 })
	org := uuid.New()
	ctx := admin(org)

	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.roles.Create(ctx, roles.Input{Name: fmt.Sprintf("Role %d", i), Permissions: []appkit.Permission{notesRead}})
		})
	}
	wg.Wait()

	created := 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case kind(err) != appkit.KindValidation:
			t.Errorf("Create bersamaan = %v", err)
		}
	}
	l, err := f.roles.List(ctx)
	if err != nil || created != 3 || l.Custom.Count != 3 {
		t.Errorf("dibuat %d, tersimpan %d, %v; ingin 3", created, l.Custom.Count, err)
	}
}

// Katalog berubah bersama rilis, sedangkan baris role tidak. Yang tersimpan
// disaring saat dibaca: izin yang hilang, yang menjadi sensitif, dan yang
// berpindah audiens tidak lagi berlaku; izin baru tidak masuk sendiri.
func TestStoredPermissionsFollowTheCatalog(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	r := f.create(t, ctx, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead, notesWrite}})
	ext := f.create(t, ctx, roles.Input{Name: "Pemesan", Audience: roles.AudienceExternal, Permissions: []appkit.Permission{portalView, portalOrder}})

	// "Rilis berikutnya": notes.write menjadi sensitif, portal.order hilang,
	// dan ada izin baru.
	opts := f.options()
	opts.Permissions = []roles.Definition{
		{Name: roles.Manage, Label: "Mengelola role", Sensitive: true},
		{Name: notesRead, Label: "Melihat catatan"},
		{Name: notesWrite, Label: "Mengubah catatan", Sensitive: true},
		{Name: "notes.export", Label: "Mengekspor catatan"},
		{Name: portalView, Label: "Melihat pesanan sendiri", Audience: roles.AudienceExternal},
	}
	opts.Builtins = []roles.Builtin{{Key: "administrator", Name: "Administrator", Administrator: true}}
	next, err := roles.New(f.pool, f.trail, testdb.Hooks(), opts)
	if err != nil {
		t.Fatal(err)
	}

	bg := context.Background()
	if got, err := next.PermissionsOf(bg, org, r.Key); err != nil || !slices.Equal(got, []appkit.Permission{notesRead}) {
		t.Errorf("izin role internal sesudah rilis = %v, %v", got, err)
	}
	if got, err := next.PermissionsOf(bg, org, ext.Key); err != nil || !slices.Equal(got, []appkit.Permission{portalView}) {
		t.Errorf("izin role orang luar sesudah rilis = %v, %v", got, err)
	}
	// Administrator langsung memegang izin baru; role buatan tidak.
	if got, _ := next.PermissionsOf(bg, org, "administrator"); !slices.Contains(got, "notes.export") {
		t.Errorf("administrator tidak memegang izin baru: %v", got)
	}
	// Role bawaan yang dihapus dari kode tidak memegang izin apa pun.
	if got, _ := next.PermissionsOf(bg, org, "staff"); len(got) != 0 {
		t.Errorf("role bawaan yang sudah dihapus = %v", got)
	}

	// Menyimpan ulang tidak dapat mempertahankan izin yang kini terlarang.
	_, err = next.Update(ctx, r.Key, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead, notesWrite}, Version: r.Version})
	if fields := fieldErrors(t, err); fields["permissions"] == "" {
		t.Errorf("izin yang kini sensitif tersimpan ulang: %v", fields)
	}
}

// Menghapus role dan memberikannya ke pengguna tidak pernah berselang: Delete
// mengambil kunci pemberian role milik produk SEBELUM menghitung pemegangnya,
// di transaksi penghapusannya.
func TestDeleteLocksAssignments(t *testing.T) {
	org := uuid.New()
	var (
		order  []string
		locked uuid.UUID
		fail   error
	)
	var f *fixture
	f = setup(t, func(o *roles.Options) {
		counts := o.UserCounts
		o.UserCounts = func(ctx context.Context, org uuid.UUID) (map[string]int, error) {
			order = append(order, "count")
			return counts(ctx, org)
		}
		o.LockAssignments = func(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
			order = append(order, "lock")
			locked = org
			if fail != nil {
				return fail
			}
			// Kunci diambil di transaksi penghapusan, bukan koneksi lain.
			_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('users.grant.' || $1::text))`, org)
			return err
		}
	})
	ctx := admin(org)
	r := f.create(t, ctx, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})

	// Kunci yang gagal diambil membatalkan penghapusan.
	fail = errors.New("kunci tidak didapat")
	if err := f.roles.Delete(ctx, r.Key); !errors.Is(err, fail) {
		t.Errorf("Delete saat kunci gagal = %v", err)
	}
	if _, err := f.roles.Get(context.Background(), org, r.Key); err != nil {
		t.Errorf("role terhapus walau kunci gagal: %v", err)
	}

	fail, order = nil, nil
	if err := f.roles.Delete(ctx, r.Key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !slices.Equal(order, []string{"lock", "count"}) || locked != org {
		t.Errorf("urutan = %v untuk %s, ingin kunci lalu hitung", order, locked)
	}

	// List tidak mengunci: ia hanya membaca.
	order = nil
	if _, err := f.roles.List(ctx); err != nil || !slices.Equal(order, []string{"count"}) {
		t.Errorf("List = %v, urutan %v", err, order)
	}
}

// Setiap perubahan role masuk jejak audit, di transaksi yang sama.
func TestChangesAreRecorded(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	creator, editor := uuid.New(), uuid.New()
	as := func(user uuid.UUID) context.Context {
		return testdb.With(context.Background(), testdb.Session{
			Organization: org, User: user, Permissions: []appkit.Permission{roles.Manage},
		})
	}

	r := f.create(t, as(creator), roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})
	if _, err := f.roles.Update(as(editor), r.Key, roles.Input{Name: "Kasir Senior", Permissions: []appkit.Permission{notesRead, notesWrite}, Version: r.Version}); err != nil {
		t.Fatal(err)
	}
	if err := f.roles.Delete(as(editor), r.Key); err != nil {
		t.Fatal(err)
	}

	events := testdb.Recorded(t, f.trail, org)
	if len(events) != 3 {
		t.Fatalf("catatan = %+v", events)
	}
	for i, want := range []struct {
		action  string
		actor   uuid.UUID
		summary string
		details string
	}{
		{roles.ActionCreated, creator, "Role “Kasir” dibuat.",
			`{"after":{"audience":"internal","description":"","name":"Kasir","permissions":["notes.read"]}}`},
		{roles.ActionUpdated, editor, "Role “Kasir Senior” diubah.",
			`{"after":{"audience":"internal","description":"","name":"Kasir Senior","permissions":["notes.read","notes.write"]},` +
				`"before":{"audience":"internal","description":"","name":"Kasir","permissions":["notes.read"]}}`},
		// Catatan bertahan setelah rolenya dihapus, dan menyebut isi terakhirnya.
		{roles.ActionDeleted, editor, "Role “Kasir Senior” dihapus.",
			`{"before":{"audience":"internal","description":"","name":"Kasir Senior","permissions":["notes.read","notes.write"]}}`},
	} {
		e := events[i]
		details, _ := json.Marshal(e.Details)
		if e.Action != want.action || e.Category != audit.CategoryAccess || e.ActorID == nil || *e.ActorID != want.actor ||
			e.Target != (audit.Target{Type: roles.TargetType, ID: r.Key}) || e.Summary != want.summary || string(details) != want.details {
			t.Errorf("catatan %d = %+v\nrincian %s", i, e, details)
		}
	}
}

func TestRoutes(t *testing.T) {
	f := setup(t)
	org := uuid.New()

	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", f.roles.Routes()...)

	do := func(ctx context.Context, method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/roles"},
		{http.MethodPost, "/v1/roles"},
		{http.MethodPut, "/v1/roles/" + uuid.NewString()},
		{http.MethodDelete, "/v1/roles/" + uuid.NewString()},
	} {
		// Body sengaja bukan JSON: izin diperiksa sebelum body dibaca.
		if code, _ := do(staff(org), tc.method, tc.path, "bukan json"); code != http.StatusForbidden {
			t.Errorf("%s %s tanpa izin = %d, ingin 403", tc.method, tc.path, code)
		}
		if code, _ := do(context.Background(), tc.method, tc.path, "{}"); code != http.StatusUnauthorized {
			t.Errorf("%s %s tanpa sesi = %d, ingin 401", tc.method, tc.path, code)
		}
	}

	code, raw := do(admin(org), http.MethodGet, "/v1/roles", "")
	if code != http.StatusOK {
		t.Fatalf("GET /roles = %d, %s", code, raw)
	}
	// Bentuk JSON-nya tetap lengkap: klien tidak perlu menebak field yang hilang.
	for _, field := range []string{
		`"key":"administrator"`, `"builtin":true`, `"updated_at":null`, `"users":{`,
		`"name":"settings.roles.manage"`, `"sensitive":true`, `"audience":"external"`,
		`"custom":{"enabled":true,"count":0,"max":30}`,
	} {
		if !strings.Contains(raw, field) {
			t.Errorf("GET /roles tidak memuat %s: %s", field, raw)
		}
	}

	code, raw = do(admin(org), http.MethodPost, "/v1/roles", `{"name":"Kasir","permissions":["notes.read"]}`)
	var created roles.Role
	if err := json.Unmarshal([]byte(raw), &created); err != nil || code != http.StatusCreated || created.Name != "Kasir" || created.Version != 1 {
		t.Fatalf("POST /roles = %d, %s", code, raw)
	}
	path := "/v1/roles/" + created.Key

	// Field yang tidak dikenal ditolak, bukan diabaikan.
	code, raw = do(admin(org), http.MethodPost, "/v1/roles", `{"name":"A","izin":[]}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, `"izin"`) {
		t.Errorf("field tak dikenal = %d, %s", code, raw)
	}
	code, raw = do(admin(org), http.MethodPost, "/v1/roles", `{"name":"A","permissions":["settings.users.manage"]}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, `"permissions"`) {
		t.Errorf("izin sensitif = %d, %s", code, raw)
	}

	code, raw = do(admin(org), http.MethodPut, path, `{"name":"Kasir Senior","permissions":["notes.read","notes.write"],"version":1}`)
	if code != http.StatusOK || !strings.Contains(raw, `"version":2`) || !strings.Contains(raw, `"name":"Kasir Senior"`) {
		t.Errorf("PUT = %d, %s", code, raw)
	}
	if code, raw := do(admin(org), http.MethodPut, path, `{"name":"Timpa","permissions":["notes.read"],"version":1}`); code != http.StatusConflict {
		t.Errorf("PUT dengan version lama = %d, %s", code, raw)
	}
	if code, raw := do(admin(org), http.MethodPut, "/v1/roles/administrator", `{"name":"Lain","permissions":["notes.read"]}`); code != http.StatusBadRequest {
		t.Errorf("PUT role bawaan = %d, %s", code, raw)
	}
	if code, _ := do(admin(uuid.New()), http.MethodPut, path, `{"name":"Dibajak","permissions":["notes.read"],"version":2}`); code != http.StatusNotFound {
		t.Errorf("PUT lintas organization = %d, ingin 404", code)
	}
	if code, _ := do(admin(uuid.New()), http.MethodDelete, path, ""); code != http.StatusNotFound {
		t.Errorf("DELETE lintas organization = %d, ingin 404", code)
	}

	f.disable(org, true)
	if code, raw := do(admin(org), http.MethodPost, "/v1/roles", `{"name":"Gudang","permissions":["notes.read"]}`); code != http.StatusPaymentRequired {
		t.Errorf("POST tanpa hak pakai = %d, %s", code, raw)
	}
	f.disable(org, false)

	if code, raw := do(admin(org), http.MethodDelete, path, ""); code != http.StatusNoContent || raw != "" {
		t.Errorf("DELETE = %d, %q", code, raw)
	}
	if code, _ := do(admin(org), http.MethodDelete, path, ""); code != http.StatusNotFound {
		t.Errorf("DELETE kedua = %d, ingin 404", code)
	}

	if actions := testdb.Actions(t, f.trail, org); !slices.Equal(actions, []string{roles.ActionCreated, roles.ActionUpdated, roles.ActionDeleted}) {
		t.Errorf("tindakan tercatat = %v", actions)
	}
}

// Susunan yang melanggar pagar gagal saat start, bukan saat dipakai.
func TestNewRejectsInvalidSetup(t *testing.T) {
	pool := testdb.New(t)
	trail := testdb.Trail(t, pool)
	f := &fixture{}

	if _, err := roles.New(nil, trail, testdb.Hooks(), f.options()); err == nil {
		t.Error("New tanpa pool lolos")
	}
	if _, err := roles.New(pool, nil, testdb.Hooks(), f.options()); err == nil {
		t.Error("New tanpa jejak audit lolos")
	}
	if _, err := roles.New(pool, trail, appkit.Hooks{}, f.options()); err == nil {
		t.Error("New tanpa pengait lolos")
	}

	for name, change := range map[string]func(*roles.Options){
		"tanpa CustomEnabled": func(o *roles.Options) { o.CustomEnabled = nil },
		"tanpa UserCounts":    func(o *roles.Options) { o.UserCounts = nil },
		"tanpa katalog":       func(o *roles.Options) { o.Permissions = nil },
		"tanpa role bawaan":   func(o *roles.Options) { o.Builtins = nil },
		"izin tanpa nama":     func(o *roles.Options) { o.Permissions = append(o.Permissions, roles.Definition{Label: "A"}) },
		"izin berspasi": func(o *roles.Options) {
			o.Permissions = append(o.Permissions, roles.Definition{Name: "notes delete", Label: "A"})
		},
		"izin tanpa label": func(o *roles.Options) { o.Permissions = append(o.Permissions, roles.Definition{Name: "notes.delete"}) },
		"izin kembar": func(o *roles.Options) {
			o.Permissions = append(o.Permissions, roles.Definition{Name: notesRead, Label: "A"})
		},
		"audiens izin salah": func(o *roles.Options) {
			o.Permissions = append(o.Permissions, roles.Definition{Name: "notes.delete", Label: "A", Audience: "publik"})
		},
		"izin sensitif untuk orang luar": func(o *roles.Options) {
			o.Permissions = append(o.Permissions, roles.Definition{Name: "portal.pay", Label: "A", Audience: roles.AudienceExternal, Sensitive: true})
		},
		"izin sensitif untuk mesin": func(o *roles.Options) {
			o.Permissions = append(o.Permissions, roles.Definition{Name: "api.users.manage", Label: "A", Audience: roles.AudienceMachine, Sensitive: true})
		},
		"role bawaan beraudiens mesin": func(o *roles.Options) {
			o.Builtins = append(o.Builtins, roles.Builtin{Key: "robot", Name: "Robot", Audience: roles.AudienceMachine})
		},
		"role bawaan dengan izin mesin": func(o *roles.Options) {
			o.Builtins[1].Permissions = []appkit.Permission{apiOrders}
		},
		"katalog tanpa izin mengelola role":  func(o *roles.Options) { o.Permissions = slices.Delete(o.Permissions, 1, 2) },
		"izin mengelola role tidak sensitif": func(o *roles.Options) { o.Permissions[1].Sensitive = false },
		"tanpa administrator":                func(o *roles.Options) { o.Builtins = o.Builtins[1:] },
		"dua administrator": func(o *roles.Options) {
			o.Builtins = append(o.Builtins, roles.Builtin{Key: "owner", Name: "Pemilik", Administrator: true})
		},
		"administrator untuk orang luar": func(o *roles.Options) { o.Builtins[0].Audience = roles.AudienceExternal },
		"administrator dengan daftar izin": func(o *roles.Options) {
			o.Builtins[0].Permissions = []appkit.Permission{notesRead}
		},
		"key berhuruf besar":        func(o *roles.Options) { o.Builtins[1].Key = "Staff" },
		"key berbentuk uuid":        func(o *roles.Options) { o.Builtins[1].Key = uuid.NewString() },
		"key kembar":                func(o *roles.Options) { o.Builtins[1].Key = "administrator" },
		"role bawaan tanpa nama":    func(o *roles.Options) { o.Builtins[1].Name = " " },
		"nama role bawaan kembar":   func(o *roles.Options) { o.Builtins[1].Name = "ADMINISTRATOR" },
		"audiens role bawaan salah": func(o *roles.Options) { o.Builtins[1].Audience = "publik" },
		"role bawaan dengan izin sensitif": func(o *roles.Options) {
			o.Builtins[1].Permissions = []appkit.Permission{usersManage}
		},
		"role bawaan dengan izin tak dikenal": func(o *roles.Options) {
			o.Builtins[1].Permissions = []appkit.Permission{"notes.delete"}
		},
		"role bawaan internal dengan izin orang luar": func(o *roles.Options) {
			o.Builtins[1].Permissions = []appkit.Permission{portalView}
		},
		"role bawaan orang luar dengan izin internal": func(o *roles.Options) {
			o.Builtins[2].Permissions = []appkit.Permission{notesRead}
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := f.options()
			change(&opts)
			if _, err := roles.New(pool, trail, testdb.Hooks(), opts); err == nil {
				t.Error("susunan yang tidak sah lolos")
			}
		})
	}

	// Role bawaan tanpa izin sama sekali sah: ia hanya dapat masuk.
	opts := f.options()
	opts.Builtins = append(opts.Builtins, roles.Builtin{Key: "guest", Name: "Tamu"})
	if _, err := roles.New(pool, trail, testdb.Hooks(), opts); err != nil {
		t.Errorf("role bawaan tanpa izin ditolak: %v", err)
	}
}
