package users_test

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
	"github.com/gonsutrijayautama/gonsu-appkit-go/users"
)

const (
	notesRead  appkit.Permission = "notes.read"
	portalView appkit.Permission = "portal.view"
)

// fixture memegang yang biasanya dijawab produk: batas pengguna, penyedia
// identitas, dan pencabutan sesi.
type fixture struct {
	pool   *pgxpool.Pool
	trail  *audit.Service
	roles  *roles.Service
	people *users.Service

	mu sync.Mutex
	// seats per organization; tanpa isian berarti tanpa batas.
	seats map[uuid.UUID]int64
	// accounts: email → akun di "penyedia identitas".
	accounts    map[string]users.Identity
	provisioned []string
	provisionOK bool
	provisionEr error
	revoked     []uuid.UUID
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		pool: testdb.New(t), seats: map[uuid.UUID]int64{}, accounts: map[string]users.Identity{}, provisionOK: true,
	}
	f.trail = testdb.Trail(t, f.pool)
	var err error
	// roles dan users saling membutuhkan: roles bertanya jumlah pemegang role
	// ke users, users memeriksa role ke roles. Penutup di bawah memutusnya.
	f.roles, err = roles.New(f.pool, f.trail, testdb.Hooks(), roles.Options{
		Permissions: []roles.Definition{
			{Name: users.Manage, Label: "Mengelola pengguna", Sensitive: true},
			{Name: roles.Manage, Label: "Mengelola role", Sensitive: true},
			{Name: notesRead, Label: "Melihat catatan"},
			{Name: portalView, Label: "Melihat pesanan sendiri", Audience: roles.AudienceExternal},
		},
		Builtins: []roles.Builtin{
			{Key: "administrator", Name: "Administrator", Administrator: true},
			{Key: "staff", Name: "Staf", Permissions: []appkit.Permission{notesRead}},
			{Key: "customer", Name: "Customer", Audience: roles.AudienceExternal, Permissions: []appkit.Permission{portalView}},
		},
		CustomEnabled: func(context.Context, uuid.UUID) (bool, error) { return true, nil },
		UserCounts: func(ctx context.Context, org uuid.UUID) (map[string]int, error) {
			return f.people.CountByRole(ctx, org)
		},
		LockAssignments: func(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
			return f.people.LockGrants(ctx, tx, org)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.people, err = users.New(f.pool, f.roles, f.trail, testdb.Hooks(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) options() users.Options {
	return users.Options{
		Seats: func(_ context.Context, org uuid.UUID) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if limit, ok := f.seats[org]; ok {
				return limit, nil
			}
			return users.Unlimited, nil
		},
		Provision: func(_ context.Context, email, name string) (users.Identity, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.provisioned = append(f.provisioned, email)
			if f.provisionEr != nil {
				return users.Identity{}, f.provisionEr
			}
			if id, ok := f.accounts[email]; ok {
				return id, nil
			}
			// Akun baru: sandi sementara hanya dikembalikan sekali.
			id := users.Identity{Subject: "sub-" + email, Email: email, Name: name}
			f.accounts[email] = id
			id.TemporaryPassword = "sandi-sementara"
			return id, nil
		},
		Available: func(context.Context) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.provisionOK
		},
		RevokeSessions: func(_ context.Context, tx pgx.Tx, _, user uuid.UUID) error {
			if tx == nil {
				return errors.New("tanpa transaksi")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.revoked = append(f.revoked, user)
			return nil
		},
	}
}

func (f *fixture) limit(org uuid.UUID, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seats[org] = n
}

var operator = users.Actor{Source: users.SourceOperator}

// grant memberi akses lewat jalan operator.
func (f *fixture) grant(t *testing.T, org uuid.UUID, subject, role string) users.User {
	t.Helper()
	u, err := f.people.Grant(context.Background(), org, users.GrantInput{
		Subject: subject, Email: subject + "@contoh.example", Name: "Nama " + subject, Role: role, Actor: operator,
	})
	if err != nil {
		t.Fatalf("Grant %s: %v", subject, err)
	}
	return u
}

// as: sesi pengguna u dengan izin perms.
func as(org uuid.UUID, u users.User, perms ...appkit.Permission) context.Context {
	return testdb.With(context.Background(), testdb.Session{Organization: org, User: u.ID, Permissions: perms})
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

func TestGrant(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	bg := context.Background()

	u, err := f.people.Grant(bg, org, users.GrantInput{
		Subject: " sub-1 ", Email: " ani@contoh.example ", Name: "  Ani \t Wijaya ", Role: "staff", Actor: operator,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if u.Subject != "sub-1" || u.Email != "ani@contoh.example" || u.Name != "Ani Wijaya" ||
		u.Role != "staff" || u.Status != users.StatusActive || u.LastLoginAt != nil {
		t.Errorf("pengguna = %+v", u)
	}

	// Memberi lagi dengan isi yang sama tidak mengubah dan tidak mencatat apa
	// pun; email dan nama yang kosong tidak menghapus yang tersimpan.
	again, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-1", Role: "staff", Actor: operator})
	if err != nil || again.ID != u.ID || again.Email != "ani@contoh.example" || again.Name != "Ani Wijaya" {
		t.Errorf("Grant kedua = %+v, %v", again, err)
	}
	// Role lain: role-nya berubah, orangnya sama.
	changed, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-1", Role: "administrator", Actor: users.Actor{Source: users.SourceOwner}})
	if err != nil || changed.ID != u.ID || changed.Role != "administrator" {
		t.Errorf("Grant dengan role lain = %+v, %v", changed, err)
	}

	events := testdb.Recorded(t, f.trail, org)
	if len(events) != 2 {
		t.Fatalf("catatan = %v", testdb.Actions(t, f.trail, org))
	}
	granted, roleChanged := events[0], events[1]
	raw, _ := json.Marshal([]any{granted.Details, roleChanged.Details})
	if granted.Action != users.ActionGranted || granted.Category != audit.CategoryAccess || granted.ActorID != nil ||
		granted.Target != (audit.Target{Type: users.TargetType, ID: u.ID.String()}) ||
		granted.Summary != "Akses diberikan kepada Ani Wijaya sebagai Staf." ||
		roleChanged.Action != users.ActionRoleChanged ||
		string(raw) != `[{"role":"staff","source":"operator"},{"role_after":"administrator","role_before":"staff","source":"owner"}]` {
		t.Errorf("catatan = %+v\n%+v\nrincian %s", granted, roleChanged, raw)
	}

	for name, in := range map[string]users.GrantInput{
		"subject kosong":       {Role: "staff", Actor: operator},
		"subject berspasi":     {Subject: "sub 2", Role: "staff", Actor: operator},
		"role tak dikenal":     {Subject: "sub-2", Role: "pemilik", Actor: operator},
		"role buatan hilang":   {Subject: "sub-2", Role: uuid.NewString(), Actor: operator},
		"nama terlalu panjang": {Subject: "sub-2", Role: "staff", Name: strings.Repeat("a", 201), Actor: operator},
	} {
		if _, err := f.people.Grant(bg, org, in); kind(err) != appkit.KindValidation {
			t.Errorf("Grant %s = %v, ingin galat validasi", name, err)
		}
	}
	// Tanpa jalan yang disebut dan tanpa organization: galat pemrogram.
	if _, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-2", Role: "staff"}); err == nil || kind(err) != "" {
		t.Errorf("Grant tanpa Actor.Source = %v", err)
	}
	if _, err := f.people.Grant(bg, uuid.Nil, users.GrantInput{Subject: "sub-2", Role: "staff", Actor: operator}); err == nil {
		t.Error("Grant tanpa organization lolos")
	}
	if all, _ := f.people.All(bg, org); len(all) != 1 {
		t.Errorf("pemberian yang ditolak tersimpan: %+v", all)
	}
}

func TestSeats(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	bg := context.Background()
	f.limit(org, 2)

	a := f.grant(t, org, "sub-a", "administrator")
	f.grant(t, org, "sub-b", "staff")
	_, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-c", Role: "staff", Actor: operator})
	if kind(err) != appkit.KindQuotaExceeded || !strings.Contains(err.Error(), "2 dari 2") {
		t.Errorf("Grant di atas batas = %v", err)
	}
	// Orang yang sudah aktif tidak dihitung ulang.
	if _, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-b", Role: "administrator", Actor: operator}); err != nil {
		t.Errorf("mengubah role saat batas penuh = %v", err)
	}
	// Organization lain punya batasnya sendiri.
	f.grant(t, uuid.New(), "sub-c", "staff")

	// Menonaktifkan membuka tempat; mengaktifkan kembali memakainya lagi.
	if err := f.people.Suspend(bg, org, "sub-b", operator); err != nil {
		t.Fatal(err)
	}
	c := f.grant(t, org, "sub-c", "staff")
	if _, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-b", Role: "staff", Actor: operator}); kind(err) != appkit.KindQuotaExceeded {
		t.Errorf("mengaktifkan kembali saat batas penuh = %v", err)
	}
	admin := as(org, a, users.Manage)
	b, _ := f.people.BySubject(bg, org, "sub-b")
	if _, err := f.people.Update(admin, b.ID, users.UpdateInput{Status: users.StatusActive}); kind(err) != appkit.KindQuotaExceeded {
		t.Errorf("Update mengaktifkan kembali saat batas penuh = %v", err)
	}
	if l, err := f.people.List(admin); err != nil || l.Seats.Active != 2 || l.Seats.Max == nil || *l.Seats.Max != 2 {
		t.Errorf("seats = %+v, %v", l.Seats, err)
	}
	_ = c

	// NOL berarti tidak boleh ada pengguna aktif baru, bukan tanpa batas.
	empty := uuid.New()
	f.limit(empty, 0)
	if _, err := f.people.Grant(bg, empty, users.GrantInput{Subject: "sub-a", Role: "administrator", Actor: operator}); kind(err) != appkit.KindQuotaExceeded {
		t.Errorf("Grant dengan batas nol = %v", err)
	}
}

// Dua pemberian akses bersamaan tidak sama-sama lolos melewati batas.
func TestSeatsHoldUnderConcurrency(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	f.limit(org, 3)

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.people.Grant(context.Background(), org, users.GrantInput{
				Subject: fmt.Sprintf("sub-%d", i), Role: "staff", Actor: operator,
			})
		})
	}
	wg.Wait()
	granted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			granted++
		case kind(err) != appkit.KindQuotaExceeded:
			t.Errorf("Grant bersamaan = %v", err)
		}
	}
	all, _ := f.people.All(context.Background(), org)
	if granted != 3 || len(all) != 3 {
		t.Errorf("diberi %d, tersimpan %d; ingin 3", granted, len(all))
	}
}

func TestSuspend(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	bg := context.Background()
	u := f.grant(t, org, "sub-1", "staff")

	if err := f.people.Suspend(bg, org, "sub-1", operator); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	got, err := f.people.ByID(bg, org, u.ID)
	if err != nil || got.Status != users.StatusSuspended || got.Role != "staff" {
		t.Errorf("pengguna sesudah Suspend = %+v, %v", got, err)
	}
	// Sesinya dicabut di transaksi yang sama.
	if !slices.Equal(f.revoked, []uuid.UUID{u.ID}) {
		t.Errorf("sesi yang dicabut = %v", f.revoked)
	}
	// Yang sudah nonaktif bukan galat, dan tidak dicatat dua kali.
	if err := f.people.Suspend(bg, org, "sub-1", operator); err != nil {
		t.Errorf("Suspend kedua = %v", err)
	}
	if got := testdb.Actions(t, f.trail, org); !slices.Equal(got, []string{users.ActionGranted, users.ActionSuspended}) {
		t.Errorf("tindakan tercatat = %v", got)
	}
	if err := f.people.Suspend(bg, org, "sub-tak-ada", operator); kind(err) != appkit.KindNotFound {
		t.Errorf("Suspend orang yang tidak ada = %v", err)
	}
	if err := f.people.Suspend(bg, uuid.New(), "sub-1", operator); kind(err) != appkit.KindNotFound {
		t.Errorf("Suspend lintas organization = %v", err)
	}
	if err := f.people.Suspend(bg, org, "sub-1", users.Actor{}); err == nil {
		t.Error("Suspend tanpa Actor.Source lolos")
	}
}

func TestRecordLogin(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	bg := context.Background()
	u := f.grant(t, org, "sub-1", "staff")

	got, err := f.people.RecordLogin(bg, org, "sub-1", "baru@contoh.example", "")
	if err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}
	// Email mengikuti penyedia identitas; nama yang kosong tidak menghapus.
	if got.ID != u.ID || got.Email != "baru@contoh.example" || got.Name != "Nama sub-1" || got.LastLoginAt == nil {
		t.Errorf("pengguna sesudah login = %+v", got)
	}
	events := testdb.Recorded(t, f.trail, org)
	last := events[len(events)-1]
	if last.Action != users.ActionSignedIn || last.Category != audit.CategorySession || last.ActorID == nil || *last.ActorID != u.ID {
		t.Errorf("catatan masuk = %+v", last)
	}

	// Orang yang tidak dikenal, milik organization lain, atau dinonaktifkan
	// tidak dapat masuk, dan percobaannya tidak dicatat sebagai "masuk".
	if _, err := f.people.RecordLogin(bg, org, "sub-asing", "", ""); kind(err) != appkit.KindNotFound {
		t.Errorf("RecordLogin orang tak dikenal = %v", err)
	}
	if _, err := f.people.RecordLogin(bg, uuid.New(), "sub-1", "", ""); kind(err) != appkit.KindNotFound {
		t.Errorf("RecordLogin lintas organization = %v", err)
	}
	if err := f.people.Suspend(bg, org, "sub-1", operator); err != nil {
		t.Fatal(err)
	}
	if _, err := f.people.RecordLogin(bg, org, "sub-1", "", ""); kind(err) != appkit.KindNotFound {
		t.Errorf("RecordLogin pengguna nonaktif = %v", err)
	}
	if got := testdb.Actions(t, f.trail, org); !slices.Equal(got, []string{users.ActionGranted, users.ActionSignedIn, users.ActionSuspended}) {
		t.Errorf("tindakan tercatat = %v", got)
	}
}

func TestListAndInvite(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	bg := context.Background()
	boss := f.grant(t, org, "sub-boss", "administrator")
	admin := as(org, boss, users.Manage)

	l, err := f.people.List(admin)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Data) != 1 || !l.Data[0].IsSelf || l.Seats.Active != 1 || l.Seats.Max != nil ||
		!l.Invite.Available || l.Invite.Reason != "" || len(l.Roles) != 3 || l.Roles[0].Name != "Administrator" {
		t.Errorf("List = %+v", l)
	}

	// Akun baru: sandi sementara tampil sekali.
	got, err := f.people.Invite(admin, users.InviteInput{Email: " budi@contoh.example ", Name: "Budi  Santoso", Role: "staff"})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if got.Account != users.AccountCreated || got.TemporaryPassword != "sandi-sementara" || got.User.IsSelf ||
		got.User.Email != "budi@contoh.example" || got.User.Name != "Budi Santoso" || got.User.Role != "staff" {
		t.Errorf("Invite = %+v", got)
	}
	// Sandi sementara tidak pernah masuk jejak audit, dan pelakunya tercatat.
	events := testdb.Recorded(t, f.trail, org)
	last := events[len(events)-1]
	if raw, _ := json.Marshal(events); strings.Contains(string(raw), "sandi-sementara") {
		t.Errorf("sandi sementara masuk jejak audit: %s", raw)
	}
	if last.Action != users.ActionGranted || last.ActorID == nil || *last.ActorID != boss.ID || last.Details["source"] != users.SourceScreen {
		t.Errorf("catatan undangan = %+v", last)
	}

	// Orang yang sudah aktif tidak diundang dua kali.
	_, err = f.people.Invite(admin, users.InviteInput{Email: "budi@contoh.example", Name: "Budi", Role: "administrator"})
	if fields := fieldErrors(t, err); !strings.Contains(fields["email"], "sebagai Staf") {
		t.Errorf("mengundang orang yang sudah aktif = %v", fields)
	}
	// Orang yang dinonaktifkan diaktifkan kembali; akunnya sudah ada, jadi
	// tanpa sandi baru.
	if err := f.people.Suspend(bg, org, "sub-budi@contoh.example", operator); err != nil {
		t.Fatal(err)
	}
	back, err := f.people.Invite(admin, users.InviteInput{Email: "budi@contoh.example", Name: "Budi", Role: "administrator"})
	if err != nil || back.Account != users.AccountExisting || back.TemporaryPassword != "" ||
		back.User.ID != got.User.ID || back.User.Status != users.StatusActive || back.User.Role != "administrator" {
		t.Errorf("mengundang orang yang nonaktif = %+v, %v", back, err)
	}

	for name, tc := range map[string]struct {
		in    users.InviteInput
		field string
	}{
		"email kosong":          {users.InviteInput{Name: "A", Role: "staff"}, "email"},
		"email tanpa @":         {users.InviteInput{Email: "bukan email", Name: "A", Role: "staff"}, "email"},
		"nama kosong":           {users.InviteInput{Email: "a@contoh.example", Name: "  ", Role: "staff"}, "name"},
		"nama panjang":          {users.InviteInput{Email: "a@contoh.example", Name: strings.Repeat("a", 201), Role: "staff"}, "name"},
		"role kosong":           {users.InviteInput{Email: "a@contoh.example", Name: "A"}, "role"},
		"role tak dikenal":      {users.InviteInput{Email: "a@contoh.example", Name: "A", Role: "pemilik"}, "role"},
		"role untuk orang luar": {users.InviteInput{Email: "a@contoh.example", Name: "A", Role: "customer"}, "role"},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(f.provisioned)
			_, err := f.people.Invite(admin, tc.in)
			if fields := fieldErrors(t, err); fields[tc.field] == "" {
				t.Errorf("tidak ada galat untuk %s: %v", tc.field, fields)
			}
			// Isian yang tidak sah tidak membuat akun di penyedia identitas.
			if len(f.provisioned) != before {
				t.Error("penyedia identitas dipanggil untuk isian yang tidak sah")
			}
		})
	}
}

func TestInviteLimits(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	boss := f.grant(t, org, "sub-boss", "administrator")
	admin := as(org, boss, users.Manage)
	in := users.InviteInput{Email: "budi@contoh.example", Name: "Budi", Role: "staff"}

	// Batas penuh diperiksa SEBELUM akunnya dibuat.
	f.limit(org, 1)
	if _, err := f.people.Invite(admin, in); kind(err) != appkit.KindQuotaExceeded || len(f.provisioned) != 0 {
		t.Errorf("Invite saat batas penuh = %v, penyedia identitas dipanggil %d kali", err, len(f.provisioned))
	}
	f.limit(org, users.Unlimited)

	// Pemasangan yang belum dapat membuat akun mengatakannya terus terang.
	f.mu.Lock()
	f.provisionOK = false
	f.mu.Unlock()
	if _, err := f.people.Invite(admin, in); kind(err) != appkit.KindValidation || len(f.provisioned) != 0 {
		t.Errorf("Invite saat belum tersedia = %v", err)
	}
	if l, err := f.people.List(admin); err != nil || l.Invite.Available || l.Invite.Reason == "" {
		t.Errorf("List saat belum tersedia = %+v, %v", l.Invite, err)
	}
	f.mu.Lock()
	f.provisionOK = true
	// Galat penyedia identitas diteruskan apa adanya.
	boom := errors.New("penyedia identitas menolak")
	f.provisionEr = boom
	f.mu.Unlock()
	if _, err := f.people.Invite(admin, in); !errors.Is(err, boom) {
		t.Errorf("galat penyedia identitas = %v", err)
	}
	if all, _ := f.people.All(context.Background(), org); len(all) != 1 {
		t.Errorf("undangan yang gagal tersimpan: %+v", all)
	}

	// Tanpa Options.Provision: layar tidak dapat menambah orang, Grant tetap bisa.
	opts := f.options()
	opts.Provision = nil
	plain, err := users.New(f.pool, f.roles, f.trail, testdb.Hooks(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Invite(admin, in); kind(err) != appkit.KindValidation {
		t.Errorf("Invite tanpa Provision = %v", err)
	}
	if l, _ := plain.List(admin); l.Invite.Available {
		t.Error("List tanpa Provision menyebut undangan tersedia")
	}
}

func TestUpdate(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	bg := context.Background()
	boss := f.grant(t, org, "sub-boss", "administrator")
	staff := f.grant(t, org, "sub-staff", "staff")
	client := f.grant(t, org, "sub-client", "customer")
	admin := as(org, boss, users.Manage)

	got, err := f.people.Update(admin, staff.ID, users.UpdateInput{Role: "administrator"})
	if err != nil || got.Role != "administrator" || got.Status != users.StatusActive || got.IsSelf {
		t.Fatalf("Update role = %+v, %v", got, err)
	}
	// Menonaktifkan mencabut sesinya; role dan status boleh diubah sekaligus.
	got, err = f.people.Update(admin, staff.ID, users.UpdateInput{Role: "staff", Status: users.StatusSuspended})
	if err != nil || got.Role != "staff" || got.Status != users.StatusSuspended || !slices.Equal(f.revoked, []uuid.UUID{staff.ID}) {
		t.Fatalf("Update menonaktifkan = %+v, %v, dicabut %v", got, err, f.revoked)
	}
	got, err = f.people.Update(admin, staff.ID, users.UpdateInput{Status: users.StatusActive})
	if err != nil || got.Status != users.StatusActive {
		t.Fatalf("Update mengaktifkan kembali = %+v, %v", got, err)
	}
	want := []string{
		users.ActionGranted, users.ActionGranted, users.ActionGranted,
		users.ActionRoleChanged, users.ActionRoleChanged, users.ActionSuspended, users.ActionReactivated,
	}
	if got := testdb.Actions(t, f.trail, org); !slices.Equal(got, want) {
		t.Errorf("tindakan tercatat = %v", got)
	}
	// Perubahan yang tidak mengubah apa pun tidak dicatat.
	if _, err := f.people.Update(admin, staff.ID, users.UpdateInput{Role: "staff", Status: users.StatusActive}); err != nil {
		t.Errorf("Update tanpa perubahan = %v", err)
	}
	if got := testdb.Actions(t, f.trail, org); len(got) != len(want) {
		t.Errorf("perubahan kosong tercatat: %v", got)
	}

	// Tidak ada yang dapat menonaktifkan dirinya sendiri.
	_, err = f.people.Update(admin, boss.ID, users.UpdateInput{Status: users.StatusSuspended})
	if fields := fieldErrors(t, err); fields["status"] == "" {
		t.Errorf("menonaktifkan diri sendiri = %v", fields)
	}
	// Administrator aktif terakhir tidak dapat diturunkan...
	_, err = f.people.Update(admin, boss.ID, users.UpdateInput{Role: "staff"})
	if fields := fieldErrors(t, err); !strings.Contains(fields["role"], "minimal satu administrator") {
		t.Errorf("menurunkan administrator terakhir = %v", fields)
	}
	// ...kecuali masih ada administrator aktif lain. Yang nonaktif tidak dihitung.
	second := f.grant(t, org, "sub-second", "administrator")
	if err := f.people.Suspend(bg, org, "sub-second", operator); err != nil {
		t.Fatal(err)
	}
	if _, err := f.people.Update(admin, boss.ID, users.UpdateInput{Role: "staff"}); kind(err) != appkit.KindValidation {
		t.Errorf("administrator nonaktif dihitung: %v", err)
	}
	if _, err := f.people.Update(admin, second.ID, users.UpdateInput{Status: users.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.people.Update(admin, boss.ID, users.UpdateInput{Role: "staff"}); err != nil || got.Role != "staff" || !got.IsSelf {
		t.Errorf("menurunkan administrator saat ada yang lain = %+v, %v", got, err)
	}

	// Jenis orangnya tidak berubah: staf tidak menjadi orang luar, atau
	// sebaliknya.
	for name, tc := range map[string]struct {
		id   uuid.UUID
		role string
	}{"staf ke role orang luar": {staff.ID, "customer"}, "orang luar ke role staf": {client.ID, "staff"}} {
		_, err := f.people.Update(admin, tc.id, users.UpdateInput{Role: tc.role})
		if fields := fieldErrors(t, err); !strings.Contains(fields["role"], "jenis pengguna") {
			t.Errorf("%s = %v", name, fields)
		}
	}

	for name, in := range map[string]users.UpdateInput{
		"tanpa isian":        {},
		"status tak dikenal": {Status: "cuti"},
		"role tak dikenal":   {Role: "pemilik"},
	} {
		if _, err := f.people.Update(admin, staff.ID, in); kind(err) != appkit.KindValidation {
			t.Errorf("Update %s = %v, ingin galat validasi", name, err)
		}
	}
	if _, err := f.people.Update(admin, uuid.New(), users.UpdateInput{Role: "staff"}); kind(err) != appkit.KindNotFound {
		t.Errorf("Update pengguna yang tidak ada = %v", err)
	}
}

// Izin ditegakkan service, dan galat dari pengait diteruskan apa adanya.
func TestOnlyManageMayChange(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	boss := f.grant(t, org, "sub-boss", "administrator")
	staff := f.grant(t, org, "sub-staff", "staff")
	in := users.InviteInput{Email: "budi@contoh.example", Name: "Budi", Role: "staff"}

	for name, tc := range map[string]struct {
		ctx  context.Context
		want error
	}{
		"tanpa izin": {as(org, staff), testdb.ErrDenied},
		"tanpa sesi": {context.Background(), testdb.ErrNoSession},
		// Pemegang izin tanpa identitas pengguna: perubahan tanpa pelaku
		// tidak pernah tersimpan.
		"tanpa pengguna": {testdb.With(context.Background(), testdb.Session{Organization: org, Permissions: []appkit.Permission{users.Manage}}), testdb.ErrNoSession},
	} {
		if _, err := f.people.List(tc.ctx); !errors.Is(err, tc.want) {
			t.Errorf("List %s = %v", name, err)
		}
		if _, err := f.people.Invite(tc.ctx, in); !errors.Is(err, tc.want) {
			t.Errorf("Invite %s = %v", name, err)
		}
		if _, err := f.people.Update(tc.ctx, boss.ID, users.UpdateInput{Role: "staff"}); !errors.Is(err, tc.want) {
			t.Errorf("Update %s = %v", name, err)
		}
	}
	if len(f.provisioned) != 0 {
		t.Errorf("penyedia identitas dipanggil tanpa izin: %v", f.provisioned)
	}
	if got, _ := f.people.ByID(context.Background(), org, boss.ID); got.Role != "administrator" {
		t.Errorf("percobaan tanpa izin mengubah pengguna: %+v", got)
	}
}

func TestTenantIsolation(t *testing.T) {
	f := setup(t)
	a, b := uuid.New(), uuid.New()
	bg := context.Background()
	inA := f.grant(t, a, "sub-1", "staff")
	// Orang yang sama (subject yang sama) di organization lain adalah
	// pengguna lain.
	bossB := f.grant(t, b, "sub-1", "administrator")
	adminB := as(b, bossB, users.Manage)

	if inA.ID == bossB.ID {
		t.Fatal("dua organization berbagi baris pengguna")
	}
	if l, err := f.people.List(adminB); err != nil || len(l.Data) != 1 || l.Data[0].ID != bossB.ID {
		t.Errorf("organization B melihat pengguna A: %+v, %v", l.Data, err)
	}
	if _, err := f.people.ByID(bg, b, inA.ID); kind(err) != appkit.KindNotFound {
		t.Errorf("ByID lintas organization = %v", err)
	}
	if _, err := f.people.Update(adminB, inA.ID, users.UpdateInput{Status: users.StatusSuspended}); kind(err) != appkit.KindNotFound {
		t.Errorf("Update lintas organization = %v", err)
	}
	if u, err := f.people.BySubject(bg, a, "sub-1"); err != nil || u.ID != inA.ID || u.Role != "staff" || u.Status != users.StatusActive {
		t.Errorf("pengguna A berubah oleh organization B: %+v, %v", u, err)
	}
	if counts, err := f.people.CountByRole(bg, a); err != nil || counts["staff"] != 1 || counts["administrator"] != 0 {
		t.Errorf("CountByRole A = %v, %v", counts, err)
	}
	if got := testdb.Actions(t, f.trail, a); len(got) != 1 {
		t.Errorf("catatan A = %v", got)
	}
}

// users dan roles saling menjaga: role yang masih dipegang pengguna tidak
// dapat dihapus, dan role yang sudah dihapus tidak dapat diberikan.
func TestRolesIntegration(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	bg := context.Background()
	boss := f.grant(t, org, "sub-boss", "administrator")
	admin := as(org, boss, users.Manage, roles.Manage)

	custom, err := f.roles.Create(admin, roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})
	if err != nil {
		t.Fatal(err)
	}
	kasir := f.grant(t, org, "sub-kasir", custom.Key)
	if kasir.Role != custom.Key {
		t.Fatalf("role buatan tidak tersimpan: %+v", kasir)
	}
	// Dipegang pengguna — termasuk yang nonaktif — berarti tidak dapat dihapus.
	if err := f.people.Suspend(bg, org, "sub-kasir", operator); err != nil {
		t.Fatal(err)
	}
	if err := f.roles.Delete(admin, custom.Key); kind(err) != appkit.KindValidation || !strings.Contains(err.Error(), "1 pengguna") {
		t.Errorf("menghapus role yang dipegang pengguna nonaktif = %v", err)
	}
	l, err := f.roles.List(admin)
	if err != nil || l.Users[custom.Key] != 1 || l.Users["administrator"] != 1 || l.Users["staff"] != 0 {
		t.Errorf("jumlah pemegang = %v, %v", l.Users, err)
	}

	// Sesudah dipindah, role dapat dihapus, dan tidak dapat diberikan lagi.
	if _, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-kasir", Role: "staff", Actor: operator}); err != nil {
		t.Fatal(err)
	}
	if err := f.roles.Delete(admin, custom.Key); err != nil {
		t.Fatalf("menghapus role yang sudah tidak dipegang = %v", err)
	}
	if _, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-baru", Role: custom.Key, Actor: operator}); kind(err) != appkit.KindValidation {
		t.Errorf("memberikan role yang sudah dihapus = %v", err)
	}
	// Role buatan milik organization lain tidak dapat diberikan.
	other := uuid.New()
	bossOther := f.grant(t, other, "sub-boss", "administrator")
	foreign, err := f.roles.Create(as(other, bossOther, roles.Manage), roles.Input{Name: "Kasir", Permissions: []appkit.Permission{notesRead}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.people.Grant(bg, org, users.GrantInput{Subject: "sub-baru", Role: foreign.Key, Actor: operator}); kind(err) != appkit.KindValidation {
		t.Errorf("memberikan role organization lain = %v", err)
	}
}

func TestRoutes(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	boss := f.grant(t, org, "sub-boss", "administrator")
	staff := f.grant(t, org, "sub-staff", "staff")
	admin := as(org, boss, users.Manage)

	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", f.people.Routes()...)
	do := func(ctx context.Context, method, path, body string) (*httptest.ResponseRecorder, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx))
		return rec, rec.Body.String()
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/users"},
		{http.MethodPost, "/v1/users"},
		{http.MethodPatch, "/v1/users/" + boss.ID.String()},
	} {
		// Body sengaja bukan JSON: izin diperiksa sebelum body dibaca.
		if rec, _ := do(as(org, staff), tc.method, tc.path, "bukan json"); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s tanpa izin = %d, ingin 403", tc.method, tc.path, rec.Code)
		}
		if rec, _ := do(context.Background(), tc.method, tc.path, "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s tanpa sesi = %d, ingin 401", tc.method, tc.path, rec.Code)
		}
	}

	rec, raw := do(admin, http.MethodGet, "/v1/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /users = %d, %s", rec.Code, raw)
	}
	// Bentuk JSON-nya tetap lengkap: klien tidak perlu menebak field yang hilang.
	for _, field := range []string{
		`"subject":"sub-boss"`, `"is_self":true`, `"status":"active"`, `"last_login_at":null`,
		`"invite":{"available":true}`, `"seats":{"active":2,"max":null}`, `"roles":[{"key":"administrator"`,
	} {
		if !strings.Contains(raw, field) {
			t.Errorf("GET /users tidak memuat %s: %s", field, raw)
		}
	}

	rec, raw = do(admin, http.MethodPost, "/v1/users", `{"email":"budi@contoh.example","name":"Budi","role":"staff"}`)
	var invited users.Invited
	if err := json.Unmarshal([]byte(raw), &invited); err != nil || rec.Code != http.StatusCreated ||
		invited.Account != users.AccountCreated || invited.TemporaryPassword == "" {
		t.Fatalf("POST /users = %d, %s", rec.Code, raw)
	}
	// Jawabannya membawa sandi sementara: tidak boleh disimpan cache.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, ingin no-store", got)
	}
	// Field yang tidak dikenal ditolak, bukan diabaikan.
	if rec, raw := do(admin, http.MethodPost, "/v1/users", `{"email":"a@contoh.example","nama":"A"}`); rec.Code != http.StatusBadRequest || !strings.Contains(raw, `"nama"`) {
		t.Errorf("field tak dikenal = %d, %s", rec.Code, raw)
	}

	path := "/v1/users/" + invited.User.ID.String()
	if rec, raw := do(admin, http.MethodPatch, path, `{"status":"suspended"}`); rec.Code != http.StatusOK || !strings.Contains(raw, `"status":"suspended"`) {
		t.Errorf("PATCH = %d, %s", rec.Code, raw)
	}
	if rec, raw := do(admin, http.MethodPatch, "/v1/users/"+boss.ID.String(), `{"status":"suspended"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH menonaktifkan diri sendiri = %d, %s", rec.Code, raw)
	}
	if rec, _ := do(admin, http.MethodPatch, "/v1/users/bukan-uuid", `{"role":"staff"}`); rec.Code != http.StatusNotFound {
		t.Errorf("PATCH id salah bentuk = %d, ingin 404", rec.Code)
	}
	bossOther := f.grant(t, uuid.New(), "sub-boss", "administrator")
	if rec, _ := do(as(uuid.New(), bossOther, users.Manage), http.MethodPatch, path, `{"role":"staff"}`); rec.Code != http.StatusNotFound {
		t.Errorf("PATCH lintas organization = %d, ingin 404", rec.Code)
	}
	f.limit(org, 2)
	if rec, raw := do(admin, http.MethodPatch, path, `{"status":"active"}`); rec.Code != http.StatusPaymentRequired {
		t.Errorf("PATCH mengaktifkan saat batas penuh = %d, %s", rec.Code, raw)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	f := setup(t)
	noUser := testdb.Hooks()
	noUser.User = nil
	noRevoke := f.options()
	noRevoke.RevokeSessions = nil

	for name, build := range map[string]func() (*users.Service, error){
		"tanpa pool":        func() (*users.Service, error) { return users.New(nil, f.roles, f.trail, testdb.Hooks(), f.options()) },
		"tanpa role":        func() (*users.Service, error) { return users.New(f.pool, nil, f.trail, testdb.Hooks(), f.options()) },
		"tanpa jejak audit": func() (*users.Service, error) { return users.New(f.pool, f.roles, nil, testdb.Hooks(), f.options()) },
		"tanpa pengait": func() (*users.Service, error) {
			return users.New(f.pool, f.roles, f.trail, appkit.Hooks{}, f.options())
		},
		"tanpa Hooks.User":     func() (*users.Service, error) { return users.New(f.pool, f.roles, f.trail, noUser, f.options()) },
		"tanpa RevokeSessions": func() (*users.Service, error) { return users.New(f.pool, f.roles, f.trail, testdb.Hooks(), noRevoke) },
	} {
		if _, err := build(); err == nil {
			t.Errorf("New %s lolos", name)
		}
	}
	// Tanpa Seats dan tanpa Provision tetap sah: tanpa batas, dan tanpa undangan.
	if _, err := users.New(f.pool, f.roles, f.trail, testdb.Hooks(), users.Options{RevokeSessions: f.options().RevokeSessions}); err != nil {
		t.Errorf("New dengan opsi minimal ditolak: %v", err)
	}
}
