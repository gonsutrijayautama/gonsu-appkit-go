package pages_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/businessprofile"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/testdb"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
	"github.com/gonsutrijayautama/gonsu-appkit-go/pages"
	"github.com/gonsutrijayautama/gonsu-appkit-go/website"
)

// catalog adalah blok "produk" di test ini.
func catalog() []pages.Block {
	return []pages.Block{
		{Type: "Hero", Fields: map[string]pages.Field{
			"title": {Kind: pages.FieldText}, "image": {Kind: pages.FieldImage},
			"button": {Kind: pages.FieldObject, Fields: map[string]pages.Field{
				"label": {Kind: pages.FieldText}, "href": {Kind: pages.FieldLink},
			}},
		}},
		{Type: "Text", Fields: map[string]pages.Field{"body": {Kind: pages.FieldRichText}}},
		{Type: "Services", Fields: map[string]pages.Field{
			"items": {Kind: pages.FieldList, Fields: map[string]pages.Field{
				"title": {Kind: pages.FieldText}, "description": {Kind: pages.FieldText}, "image": {Kind: pages.FieldImage},
			}},
		}},
		{Type: "Columns", Fields: map[string]pages.Field{"left": {Kind: pages.FieldSlot}, "right": {Kind: pages.FieldSlot}}},
		{Type: "Spacer", Fields: map[string]pages.Field{"size": {Kind: pages.FieldValue}}},
	}
}

type fixture struct {
	pool  *pgxpool.Pool
	media *media.Service
	trail *audit.Service
	sites *website.Service
	pages *pages.Service
	// org adalah organization halaman publik (tanpa sesi).
	org uuid.UUID

	mu     sync.Mutex
	limits map[uuid.UUID]int64
}

func setup(t *testing.T, change ...func(*pages.Options)) *fixture {
	t.Helper()
	f := &fixture{pool: testdb.New(t), org: uuid.New(), limits: map[uuid.UUID]int64{}}
	var err error
	if f.media, err = media.New(f.pool, testdb.Hooks(), media.Options{MaxBytes: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	f.trail, err = audit.New(f.pool, testdb.Hooks(), audit.Options{
		ActorName: func(_ context.Context, _, actor uuid.UUID) string { return "Penyusun " + actor.String()[:4] },
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := businessprofile.New(f.pool, f.media, f.trail, testdb.Hooks())
	if err != nil {
		t.Fatal(err)
	}
	f.sites, err = website.New(f.pool, profiles, f.media, f.trail, testdb.Hooks(), website.Options{
		PublicOrganization: func(*http.Request) (uuid.UUID, error) { return f.org, nil },
		CacheTTL:           1, // praktis tanpa cache: test membaca perubahan langsung
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := pages.Options{
		Blocks:   catalog(),
		Reserved: []string{"/login", "/settings", "/v1", "/_halaman"},
		Limit: func(_ context.Context, org uuid.UUID) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if n, ok := f.limits[org]; ok {
				return n, nil
			}
			return pages.Unlimited, nil
		},
	}
	for _, c := range change {
		c(&opts)
	}
	if f.pages, err = pages.New(f.pool, f.sites, f.media, f.trail, testdb.Hooks(), opts); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) limit(org uuid.UUID, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits[org] = n
}

func admin(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{
		Organization: org, User: uuid.New(),
		Permissions: []appkit.Permission{pages.Manage, website.Manage},
	})
}

func staff(org uuid.UUID) context.Context {
	return testdb.With(context.Background(), testdb.Session{Organization: org, User: uuid.New()})
}

// site membuat organization org punya halaman publik (mode "site").
func (f *fixture) site(t *testing.T, org uuid.UUID) {
	t.Helper()
	if _, err := f.sites.Update(admin(org), website.Input{Mode: website.ModeSite, Tagline: "Rapi"}); err != nil {
		t.Fatal(err)
	}
}

func block(kind string, props map[string]any) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	if _, ok := props["id"]; !ok {
		props["id"] = kind + "-" + uuid.NewString()[:8]
	}
	return map[string]any{"type": kind, "props": props}
}

func doc(blocks ...map[string]any) json.RawMessage {
	content := make([]any, len(blocks))
	for i, b := range blocks {
		content[i] = b
	}
	raw, _ := json.Marshal(map[string]any{"root": map[string]any{"props": map[string]any{}}, "content": content})
	return raw
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
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

func (f *fixture) create(t *testing.T, ctx context.Context, title, path string) pages.Page {
	t.Helper()
	p, err := f.pages.Create(ctx, pages.CreateInput{Title: title, Path: path})
	if err != nil {
		t.Fatalf("Create %s: %v", path, err)
	}
	return p
}

// input menyalin halaman p menjadi Input dengan isi d.
func input(p pages.Page, d json.RawMessage) pages.Input {
	in := pages.Input{Title: p.Title, Path: p.Path, Navigation: p.Navigation, Draft: d, Version: p.Version}
	in.SEO.Title, in.SEO.Description = p.SEO.Title, p.SEO.Description
	return in
}

func TestDocumentChecks(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	p := f.create(t, ctx, "Layanan", "/layanan")
	foreign, err := f.media.Save(context.Background(), uuid.New(), bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatal(err)
	}

	nest := func(depth int) map[string]any {
		b := block("Text", nil)
		for range depth {
			b = block("Columns", map[string]any{"left": []any{b}})
		}
		return b
	}
	many := make([]map[string]any, 301)
	for i := range many {
		many[i] = block("Spacer", nil)
	}
	longList := make([]any, 51)
	for i := range longList {
		longList[i] = map[string]any{"title": "x"}
	}

	for name, d := range map[string]json.RawMessage{
		"bukan objek":              json.RawMessage(`[]`),
		"bagian tak dikenal":       json.RawMessage(`{"content":[],"root":{"props":{}},"script":"x"}`),
		"content bukan daftar":     json.RawMessage(`{"content":{},"root":{"props":{}}}`),
		"blok tak dikenal":         doc(block("Script", nil)),
		"isian tak dikenal":        doc(block("Text", map[string]any{"onload": "x"})),
		"blok tanpa id":            doc(map[string]any{"type": "Text", "props": map[string]any{"body": "x"}}),
		"teks bukan string":        doc(block("Hero", map[string]any{"title": 5})),
		"tautan javascript":        doc(block("Hero", map[string]any{"button": map[string]any{"href": "javascript:alert(1)"}})),
		"tautan http":              doc(block("Hero", map[string]any{"button": map[string]any{"href": "http://contoh.example"}})),
		"tautan protokol relatif":  doc(block("Hero", map[string]any{"button": map[string]any{"href": "//evil.example/x"}})),
		"gambar data URL":          doc(block("Hero", map[string]any{"image": "data:image/png;base64,AAAA"})),
		"gambar alamat luar":       doc(block("Hero", map[string]any{"image": "https://evil.example/a.png"})),
		"gambar organization lain": doc(block("Hero", map[string]any{"image": foreign.URL})),
		"nilai berupa objek":       doc(block("Spacer", map[string]any{"size": map[string]any{"a": 1}})),
		"terlalu dalam":            doc(nest(6)),
		"terlalu banyak blok":      doc(many...),
		"daftar terlalu panjang":   doc(block("Services", map[string]any{"items": longList})),
		"isian butir tak dikenal":  doc(block("Services", map[string]any{"items": []any{map[string]any{"price": "1"}}})),
		"root tak dikenal":         json.RawMessage(`{"content":[],"root":{"props":{"title":"x"}}}`),
		"zones bukan daftar":       json.RawMessage(`{"content":[],"root":{"props":{}},"zones":{"a:b":"x"}}`),
		"blok tak dikenal di zone": json.RawMessage(`{"content":[],"root":{"props":{}},"zones":{"a:b":[{"type":"Script","props":{"id":"x"}}]}}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.pages.Update(ctx, p.ID, input(p, d))
			if fields := fieldErrors(t, err); fields["draft"] == "" {
				t.Errorf("tidak ada galat untuk draft: %v", fields)
			}
		})
	}

	// Yang sah: blok bersarang lima tingkat, daftar, objek, nilai, dan tautan
	// yang diizinkan.
	ok := doc(
		nest(5),
		block("Hero", map[string]any{"title": "Halo", "button": map[string]any{"label": "Hubungi", "href": "/kontak"}}),
		block("Hero", map[string]any{"button": map[string]any{"href": "#harga"}}),
		block("Hero", map[string]any{"button": map[string]any{"href": "https://wa.me/628123"}}),
		block("Services", map[string]any{"items": []any{map[string]any{"title": "A", "description": "B"}}}),
		block("Spacer", map[string]any{"size": 32}),
	)
	if _, err := f.pages.Update(ctx, p.ID, input(p, ok)); err != nil {
		t.Errorf("isi yang sah ditolak: %v", err)
	}

	// Ukuran dibatasi.
	small := setup(t, func(o *pages.Options) { o.MaxDocumentBytes = 200 })
	sp := small.create(t, admin(org), "Layanan", "/layanan")
	big := doc(block("Text", map[string]any{"body": strings.Repeat("a", 300)}))
	if fields := fieldErrors(t, func() error { _, err := small.pages.Update(admin(org), sp.ID, input(sp, big)); return err }()); fields["draft"] == "" {
		t.Errorf("isi melebihi batas diterima: %v", fields)
	}
}

// Teks berformat dibersihkan saat disimpan: yang tersimpan — dan yang kelak
// tampil ke pengunjung — sudah tanpa skrip dan tanpa tautan berbahaya.
func TestRichTextIsSanitized(t *testing.T) {
	f := setup(t)
	ctx := admin(uuid.New())
	p := f.create(t, ctx, "Tentang", "/tentang")
	dirty := `<h2>Judul</h2><p onclick="x()">Halo <strong>dunia</strong> <a href="javascript:alert(1)">jahat</a> ` +
		`<a href="https://contoh.example">baik</a> <a href="/kontak">kontak</a></p><script>alert(1)</script>` +
		`<img src=x onerror=alert(1)><iframe src="https://evil.example"></iframe><ul><li>satu</li></ul>`
	got, err := f.pages.Update(ctx, p.ID, input(p, doc(block("Text", map[string]any{"body": dirty}))))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	var stored struct {
		Content []struct {
			Props struct {
				Body string `json:"body"`
			} `json:"props"`
		} `json:"content"`
	}
	if err := json.Unmarshal(got.Draft, &stored); err != nil {
		t.Fatal(err)
	}
	body := stored.Content[0].Props.Body
	for _, gone := range []string{"<script", "onclick", "javascript:", "<img", "onerror", "<iframe"} {
		if strings.Contains(body, gone) {
			t.Errorf("teks berformat masih memuat %q: %s", gone, body)
		}
	}
	for _, kept := range []string{"<h2>Judul</h2>", "<strong>dunia</strong>", `href="https://contoh.example"`, `href="/kontak"`, "<li>satu</li>"} {
		if !strings.Contains(body, kept) {
			t.Errorf("teks berformat kehilangan %q: %s", kept, body)
		}
	}
}

func TestCreateAndList(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)

	home := f.create(t, ctx, "Beranda", "/")
	about := f.create(t, ctx, "  Tentang Kami ", "/tentang-kami")
	if home.Path != "/" || home.Status != pages.StatusDraft || home.Version != 1 || home.PublishedNumber != nil {
		t.Errorf("beranda = %+v", home)
	}
	if about.Title != "Tentang Kami" || string(about.Draft) == "" {
		t.Errorf("halaman = %+v", about)
	}

	for name, tc := range map[string]struct {
		in    pages.CreateInput
		field string
	}{
		"judul kosong":         {pages.CreateInput{Title: " ", Path: "/a"}, "title"},
		"judul panjang":        {pages.CreateInput{Title: strings.Repeat("a", 81), Path: "/a"}, "title"},
		"alamat tanpa garis":   {pages.CreateInput{Title: "A", Path: "layanan"}, "path"},
		"alamat bertingkat":    {pages.CreateInput{Title: "A", Path: "/layanan/jahit"}, "path"},
		"alamat huruf besar":   {pages.CreateInput{Title: "A", Path: "/Layanan"}, "path"},
		"alamat garis ganda":   {pages.CreateInput{Title: "A", Path: "/a--b"}, "path"},
		"alamat aplikasi":      {pages.CreateInput{Title: "A", Path: "/settings"}, "path"},
		"alamat media":         {pages.CreateInput{Title: "A", Path: "/media"}, "path"},
		"alamat kerangka":      {pages.CreateInput{Title: "A", Path: "/_halaman"}, "path"},
		"alamat sudah dipakai": {pages.CreateInput{Title: "A", Path: "/tentang-kami"}, "path"},
		"beranda kedua":        {pages.CreateInput{Title: "A", Path: "/"}, "path"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.pages.Create(ctx, tc.in)
			if fields := fieldErrors(t, err); fields[tc.field] == "" {
				t.Errorf("tidak ada galat untuk %s: %v", tc.field, fields)
			}
		})
	}

	l, err := f.pages.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Data) != 2 || l.Data[0].Path != "/" || l.Data[1].Draft != nil {
		t.Errorf("daftar = %+v", l.Data)
	}
	if !l.Limit.Enabled || l.Limit.Count != 2 || l.Limit.Max != nil || l.Legacy.Available {
		t.Errorf("batas = %+v, impor = %+v", l.Limit, l.Legacy)
	}
	if got := testdb.Actions(t, f.trail, org); len(got) != 2 || got[0] != pages.ActionCreated {
		t.Errorf("tindakan tercatat = %v", got)
	}
}

func TestDraftAndPublish(t *testing.T) {
	f := setup(t)
	ctx := admin(f.org)
	f.site(t, f.org)
	p := f.create(t, ctx, "Layanan", "/layanan")

	// Halaman tanpa blok tidak dapat diterbitkan.
	if _, err := f.pages.Publish(ctx, p.ID, input(p, doc())); fieldErrors(t, err)["draft"] == "" {
		t.Errorf("menerbitkan halaman kosong = %v", err)
	}

	first := doc(block("Hero", map[string]any{"title": "Versi satu"}))
	p, err := f.pages.Update(ctx, p.ID, input(p, first))
	if err != nil || p.Status != pages.StatusDraft || p.Version != 2 {
		t.Fatalf("simpan draf = %+v, %v", p, err)
	}
	if _, _, ok, _ := f.pages.Lookup(context.Background(), f.org, "/layanan"); ok {
		t.Error("draf terlihat pengunjung")
	}

	p, err = f.pages.Publish(ctx, p.ID, input(p, first))
	if err != nil || p.Status != pages.StatusPublished || p.PublishedNumber == nil || *p.PublishedNumber != 1 || p.PublishedAt == nil {
		t.Fatalf("terbitkan = %+v, %v", p, err)
	}
	pub, _, ok, err := f.pages.Lookup(context.Background(), f.org, "/layanan/")
	if err != nil || !ok || !strings.Contains(string(pub.Data), "Versi satu") {
		t.Fatalf("halaman terbit = %+v, %v, %v", pub, ok, err)
	}

	// Draf baru tidak mengubah yang tampil sampai diterbitkan.
	p, err = f.pages.Update(ctx, p.ID, input(p, doc(block("Hero", map[string]any{"title": "Versi dua"}))))
	if err != nil || p.Status != pages.StatusChanged {
		t.Fatalf("draf sesudah terbit = %+v, %v", p, err)
	}
	if pub, _, _, _ := f.pages.Lookup(context.Background(), f.org, "/layanan"); !strings.Contains(string(pub.Data), "Versi satu") {
		t.Errorf("draf menggantikan yang tampil: %s", pub.Data)
	}

	// Simpan-bersamaan: version lama ditolak, baik simpan maupun terbit.
	stale := input(p, first)
	stale.Version = 1
	if _, err := f.pages.Update(ctx, p.ID, stale); kind(err) != appkit.KindConflict {
		t.Errorf("simpan dengan version lama = %v", err)
	}
	if _, err := f.pages.Publish(ctx, p.ID, stale); kind(err) != appkit.KindConflict {
		t.Errorf("terbitkan dengan version lama = %v", err)
	}

	// Alamat beranda tetap, dan halaman lain tidak dapat menjadi beranda.
	home := f.create(t, ctx, "Beranda", "/")
	moved := input(home, doc())
	moved.Path = "/beranda"
	if fields := fieldErrors(t, func() error { _, err := f.pages.Update(ctx, home.ID, moved); return err }()); fields["path"] == "" {
		t.Errorf("mengganti alamat beranda = %v", fields)
	}
	toHome := input(p, first)
	toHome.Path = "/"
	if fields := fieldErrors(t, func() error { _, err := f.pages.Update(ctx, p.ID, toHome); return err }()); fields["path"] == "" {
		t.Errorf("menjadikan halaman beranda = %v", fields)
	}

	// Mengganti alamat dan pengaturan langsung berlaku, dan dicatat.
	renamed := input(p, first)
	renamed.Path, renamed.Title = "/jasa", "Jasa"
	renamed.Navigation = pages.Navigation{Visible: true, Position: 3}
	if p, err = f.pages.Update(ctx, p.ID, renamed); err != nil || p.Path != "/jasa" {
		t.Fatalf("mengganti alamat = %+v, %v", p, err)
	}
	if _, _, ok, _ := f.pages.Lookup(context.Background(), f.org, "/jasa"); !ok {
		t.Error("alamat baru tidak tampil")
	}
	events := testdb.Recorded(t, f.trail, f.org)
	var settings []string
	for _, e := range events {
		if e.Action == pages.ActionSettingsUpdated {
			raw, _ := json.Marshal(e.Details)
			settings = append(settings, string(raw))
		}
	}
	if len(settings) != 1 || settings[0] != `{"fields":["title","path","navigation"]}` {
		t.Errorf("catatan pengaturan = %v", settings)
	}
	// Menyimpan draf tidak dicatat; menerbitkan dicatat.
	published := 0
	for _, e := range events {
		if e.Action == pages.ActionPublished {
			published++
		}
	}
	if published != 1 {
		t.Errorf("catatan terbit = %d, ingin 1", published)
	}
}

func TestRevisions(t *testing.T) {
	f := setup(t, func(o *pages.Options) { o.MaxRevisions = 2 })
	ctx := admin(f.org)
	p := f.create(t, ctx, "Layanan", "/layanan")

	var revisionOne json.RawMessage
	for i := 1; i <= 3; i++ {
		d := doc(block("Hero", map[string]any{"title": fmt.Sprintf("Versi %d", i)}))
		if i == 1 {
			revisionOne = d
		}
		var err error
		if p, err = f.pages.Publish(ctx, p.ID, input(p, d)); err != nil {
			t.Fatalf("terbitan %d: %v", i, err)
		}
	}
	_ = revisionOne
	revs, err := f.pages.Revisions(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Hanya dua terakhir yang disimpan; yang tampil bertanda live.
	if len(revs) != 2 || revs[0].Number != 3 || revs[1].Number != 2 || !revs[0].Live || revs[1].Live {
		t.Fatalf("riwayat = %+v", revs)
	}
	if revs[0].PublishedBy == nil || !strings.HasPrefix(revs[0].PublishedByName, "Penyusun ") || revs[0].Title != "Layanan" {
		t.Errorf("penerbit = %+v", revs[0])
	}

	// Mengembalikan terbitan 2 ke draf; yang tampil tetap terbitan 3.
	back, err := f.pages.Restore(ctx, p.ID, revs[1].ID, pages.RestoreInput{Version: p.Version})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if back.Status != pages.StatusChanged || !strings.Contains(string(back.Draft), "Versi 2") || *back.PublishedNumber != 3 {
		t.Errorf("sesudah Restore = %+v", back)
	}
	if _, err := f.pages.Restore(ctx, p.ID, revs[1].ID, pages.RestoreInput{Version: p.Version}); kind(err) != appkit.KindConflict {
		t.Errorf("Restore dengan version lama = %v", err)
	}
	if _, err := f.pages.Restore(ctx, p.ID, uuid.New(), pages.RestoreInput{Version: back.Version}); kind(err) != appkit.KindNotFound {
		t.Errorf("Restore terbitan yang tidak ada = %v", err)
	}
	// Terbitan milik halaman lain tidak dapat dikembalikan ke halaman ini.
	other := f.create(t, ctx, "Lain", "/lain")
	if _, err := f.pages.Restore(ctx, other.ID, revs[0].ID, pages.RestoreInput{Version: other.Version}); kind(err) != appkit.KindNotFound {
		t.Errorf("Restore terbitan halaman lain = %v", err)
	}
}

func TestUnpublishAndDelete(t *testing.T) {
	f := setup(t)
	ctx := admin(f.org)
	f.site(t, f.org)
	p := f.create(t, ctx, "Layanan", "/layanan")
	p, err := f.pages.Publish(ctx, p.ID, input(p, doc(block("Hero", nil))))
	if err != nil {
		t.Fatal(err)
	}

	p, err = f.pages.Unpublish(ctx, p.ID)
	if err != nil || p.Status != pages.StatusDraft || p.PublishedNumber != nil {
		t.Fatalf("Unpublish = %+v, %v", p, err)
	}
	if _, _, ok, _ := f.pages.Lookup(context.Background(), f.org, "/layanan"); ok {
		t.Error("halaman yang ditarik masih tampil")
	}
	// Menarik lagi tidak mengubah dan tidak mencatat apa pun.
	if _, err := f.pages.Unpublish(ctx, p.ID); err != nil {
		t.Errorf("Unpublish kedua = %v", err)
	}

	if err := f.pages.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.pages.Get(ctx, p.ID); kind(err) != appkit.KindNotFound {
		t.Errorf("Get sesudah Delete = %v", err)
	}
	if err := f.pages.Delete(ctx, p.ID); kind(err) != appkit.KindNotFound {
		t.Errorf("Delete kedua = %v", err)
	}
	want := []string{pages.ActionCreated, pages.ActionPublished, pages.ActionUnpublished, pages.ActionDeleted}
	var got []string
	for _, a := range testdb.Actions(t, f.trail, f.org) {
		if strings.HasPrefix(a, "page.") {
			got = append(got, a)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tindakan tercatat = %v", got)
	}
	// Alamatnya dapat dipakai lagi.
	f.create(t, ctx, "Layanan", "/layanan")
}

// Jumlah halaman mengikuti paket. Paket tanpa penyusun halaman membekukannya:
// yang terbit tetap tampil, menarik dan menghapus tetap boleh.
func TestLimit(t *testing.T) {
	f := setup(t)
	ctx := admin(f.org)
	f.site(t, f.org)
	f.limit(f.org, 2)

	a := f.create(t, ctx, "A", "/a")
	f.create(t, ctx, "B", "/b")
	_, err := f.pages.Create(ctx, pages.CreateInput{Title: "C", Path: "/c"})
	if e, _ := errors.AsType[*appkit.Error](err); e == nil || e.Kind != appkit.KindQuotaExceeded || e.Limit != appkit.LimitPages {
		t.Errorf("Create di atas batas = %v", err)
	}
	if l, _ := f.pages.List(ctx); l.Limit.Max == nil || *l.Limit.Max != 2 || l.Limit.Count != 2 {
		t.Errorf("batas = %+v", l.Limit)
	}

	// Turun paket ke batas lebih kecil: halaman yang ada tetap dapat disusun.
	f.limit(f.org, 1)
	a, err = f.pages.Publish(ctx, a.ID, input(a, doc(block("Hero", nil))))
	if err != nil {
		t.Fatalf("menerbitkan saat jumlah di atas batas baru = %v", err)
	}

	// Paket tanpa penyusun halaman.
	f.limit(f.org, 0)
	frozen := func(name string, err error) {
		t.Helper()
		if e, _ := errors.AsType[*appkit.Error](err); e == nil || e.Kind != appkit.KindQuotaExceeded || e.Limit != appkit.LimitPages {
			t.Errorf("%s tanpa paket = %v", name, err)
		}
	}
	_, err = f.pages.Create(ctx, pages.CreateInput{Title: "C", Path: "/c"})
	frozen("Create", err)
	_, err = f.pages.Update(ctx, a.ID, input(a, doc(block("Hero", nil))))
	frozen("Update", err)
	_, err = f.pages.Publish(ctx, a.ID, input(a, doc(block("Hero", nil))))
	frozen("Publish", err)
	_, err = f.pages.UploadImage(ctx, a.ID, bytes.NewReader(pngBytes(t)))
	frozen("UploadImage", err)
	revs, _ := f.pages.Revisions(ctx, a.ID)
	_, err = f.pages.Restore(ctx, a.ID, revs[0].ID, pages.RestoreInput{Version: a.Version})
	frozen("Restore", err)
	if l, err := f.pages.List(ctx); err != nil || l.Limit.Enabled {
		t.Errorf("List tanpa paket = %+v, %v", l.Limit, err)
	}
	// Yang terbit tetap tampil; menarik dan menghapus tetap boleh.
	if _, _, ok, _ := f.pages.Lookup(context.Background(), f.org, "/a"); !ok {
		t.Error("halaman terbit hilang saat paket tidak menyertakan penyusun halaman")
	}
	if _, err := f.pages.Unpublish(ctx, a.ID); err != nil {
		t.Errorf("Unpublish tanpa paket = %v", err)
	}
	if err := f.pages.Delete(ctx, a.ID); err != nil {
		t.Errorf("Delete tanpa paket = %v", err)
	}
}

func TestLimitHoldsUnderConcurrency(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	f.limit(org, 3)
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.pages.Create(ctx, pages.CreateInput{Title: "H", Path: fmt.Sprintf("/h%d", i)})
		})
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		if err == nil {
			created++
		} else if kind(err) != appkit.KindQuotaExceeded {
			t.Errorf("Create bersamaan = %v", err)
		}
	}
	if l, _ := f.pages.List(ctx); created != 3 || l.Limit.Count != 3 {
		t.Errorf("dibuat %d, tersimpan %d; ingin 3", created, l.Limit.Count)
	}
}

func TestPublicAndRender(t *testing.T) {
	f := setup(t)
	ctx := admin(f.org)
	bg := context.Background()

	home := f.create(t, ctx, "Beranda", "/")
	svc := f.create(t, ctx, "Layanan", "/layanan")
	hidden := f.create(t, ctx, "Rahasia", "/rahasia")
	draftOnly := f.create(t, ctx, "Draf", "/draf")
	publish := func(p pages.Page, nav pages.Navigation, title string) pages.Page {
		in := input(p, doc(block("Hero", map[string]any{"title": title})))
		in.Navigation = nav
		out, err := f.pages.Publish(ctx, p.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	publish(home, pages.Navigation{Visible: true, Position: 1}, "Selamat datang")
	svc = publish(svc, pages.Navigation{Visible: true, Position: 2}, `Akhir </script><script>alert(1)</script>`)
	publish(hidden, pages.Navigation{Visible: false}, "Tidak di menu")
	if _, err := f.pages.Update(ctx, draftOnly.ID, input(draftOnly, doc(block("Hero", nil)))); err != nil {
		t.Fatal(err)
	}

	// Mode "signin": tidak ada halaman yang tampil, walau terbit.
	if _, _, ok, err := f.pages.Lookup(bg, f.org, "/layanan"); ok || err != nil {
		t.Errorf("halaman tampil di mode signin: %v, %v", ok, err)
	}
	f.site(t, f.org)

	pub, site, ok, err := f.pages.Lookup(bg, f.org, "/layanan")
	if err != nil || !ok {
		t.Fatalf("Lookup = %v, %v", ok, err)
	}
	if len(pub.Navigation) != 2 || pub.Navigation[0].Path != "/" || pub.Navigation[1].Path != "/layanan" {
		t.Errorf("menu = %+v", pub.Navigation)
	}
	if pub.SEO.Title != "Layanan" || site.Mode != website.ModeSite {
		t.Errorf("SEO = %+v, mode %s", pub.SEO, site.Mode)
	}
	for _, path := range []string{"/draf", "/tidak-ada", "/LAYANAN"} {
		if _, _, ok, _ := f.pages.Lookup(bg, f.org, path); ok {
			t.Errorf("Lookup(%s) menampilkan halaman", path)
		}
	}
	// Organization lain tidak melihatnya.
	if _, _, ok, _ := f.pages.Lookup(bg, uuid.New(), "/layanan"); ok {
		t.Error("halaman tampil untuk organization lain")
	}

	shell := []byte(`<!DOCTYPE html><html><head><title>Kerangka</title></head><body></body></html>`)
	req := httptest.NewRequest(http.MethodGet, "http://usaha.example/layanan", nil)
	out, ok := f.pages.RenderPage(req, shell)
	if !ok {
		t.Fatal("RenderPage tidak menampilkan halaman terbit")
	}
	html := string(out)
	for _, want := range []string{`<title>Layanan</title>`, `id="gonsu-page"`, `id="gonsu-site"`, `"path":"/layanan"`} {
		if !strings.Contains(html, want) {
			t.Errorf("hasil RenderPage tidak memuat %s:\n%s", want, html)
		}
	}
	// Isi halaman tidak dapat menutup elemen script-nya.
	if strings.Contains(html, "</script><script>alert(1)") {
		t.Errorf("isi halaman keluar dari elemen script:\n%s", html)
	}
	if _, ok := f.pages.RenderPage(httptest.NewRequest(http.MethodGet, "/draf", nil), shell); ok {
		t.Error("RenderPage menampilkan draf")
	}

	// Endpoint publik.
	mux := http.NewServeMux()
	appkit.Register(mux, "", f.pages.PublicRoutes()...)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/page.json?path=/layanan", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"Layanan"`) || rec.Header().Get("Cache-Control") == "" {
		t.Errorf("GET /page.json = %d, %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/page.json?path=/draf", nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Hero") {
		t.Errorf("GET /page.json draf = %d, %s", rec.Code, rec.Body.String())
	}
	_ = svc
}

func TestImages(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	bg := context.Background()
	p := f.create(t, ctx, "Galeri", "/galeri")

	if _, err := f.pages.UploadImage(staff(org), p.ID, bytes.NewReader(pngBytes(t))); !errors.Is(err, testdb.ErrDenied) {
		t.Errorf("UploadImage tanpa izin = %v", err)
	}
	if _, err := f.pages.UploadImage(ctx, uuid.New(), bytes.NewReader(pngBytes(t))); kind(err) != appkit.KindNotFound {
		t.Errorf("UploadImage ke halaman yang tidak ada = %v", err)
	}
	kept, err := f.pages.UploadImage(ctx, p.ID, bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	inHistory, err := f.pages.UploadImage(ctx, p.ID, bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatal(err)
	}
	unused, err := f.pages.UploadImage(ctx, p.ID, bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.pages.UploadImage(ctx, p.ID, bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatal(err)
	}

	// Terbitkan dengan gambar yang kelak hanya ada di riwayat, lalu draf baru
	// yang memakai gambar lain.
	if p, err = f.pages.Publish(ctx, p.ID, input(p, doc(block("Hero", map[string]any{"image": inHistory.URL})))); err != nil {
		t.Fatal(err)
	}
	if p, err = f.pages.Publish(ctx, p.ID, input(p, doc(block("Hero", map[string]any{"image": kept.URL})))); err != nil {
		t.Fatal(err)
	}
	// Semua gambar dianggap sudah lewat sehari, kecuali satu yang baru.
	if _, err := f.pool.Exec(bg, `UPDATE appkit_page_media SET created_at = now() - interval '2 days'
		WHERE organization_id = $1 AND media_id <> $2`, org, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if p, err = f.pages.Update(ctx, p.ID, input(p, doc(block("Hero", map[string]any{"image": kept.URL})))); err != nil {
		t.Fatal(err)
	}
	exists := func(f2 media.File) bool { _, err := f.media.Get(bg, org, f2.ID); return err == nil }
	if !exists(kept) || !exists(inHistory) || !exists(fresh) {
		t.Errorf("gambar yang masih dirujuk atau masih baru terhapus: kept=%v history=%v fresh=%v", exists(kept), exists(inHistory), exists(fresh))
	}
	if exists(unused) {
		t.Error("gambar yang tidak dirujuk siapa pun tidak terhapus")
	}

	// Gambar pratinjau.
	got, err := f.pages.SetSEOImage(ctx, p.ID, bytes.NewReader(pngBytes(t)))
	if err != nil || got.SEO.Image == nil {
		t.Fatalf("SetSEOImage = %+v, %v", got.SEO, err)
	}
	first := *got.SEO.Image
	if got, err = f.pages.SetSEOImage(ctx, p.ID, bytes.NewReader(pngBytes(t))); err != nil || got.SEO.Image.ID == first.ID {
		t.Fatalf("mengganti gambar pratinjau = %+v, %v", got.SEO, err)
	}
	if exists(first) {
		t.Error("gambar pratinjau lama tidak terhapus")
	}
	if got, err = f.pages.RemoveSEOImage(ctx, p.ID); err != nil || got.SEO.Image != nil {
		t.Errorf("RemoveSEOImage = %+v, %v", got.SEO, err)
	}

	// Menghapus halaman menghapus gambarnya yang tidak dirujuk halaman lain.
	if err := f.pages.Delete(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if exists(kept) || exists(inHistory) {
		t.Errorf("gambar halaman yang dihapus masih ada: kept=%v history=%v", exists(kept), exists(inHistory))
	}
}

func TestLegacyImport(t *testing.T) {
	f := setup(t, func(o *pages.Options) {
		o.ImportLegacy = func(_ context.Context, legacy website.Legacy) (json.RawMessage, error) {
			items := []any{}
			for _, s := range legacy.Services {
				items = append(items, map[string]any{"title": s.Title, "description": s.Description})
			}
			hero := map[string]any{"title": "Tentang kami"}
			if legacy.AboutImage != nil {
				hero["image"] = legacy.AboutImage.URL
			}
			return doc(block("Hero", hero), block("Text", map[string]any{"body": "<p>" + legacy.AboutText + "</p>"}),
				block("Services", map[string]any{"items": items})), nil
		}
	})
	org := uuid.New()
	ctx := admin(org)
	bg := context.Background()

	// Tanpa isi lama: tidak ada tawaran.
	if l, _ := f.pages.List(ctx); l.Legacy.Available {
		t.Error("tawaran impor tanpa isi lama")
	}
	photo, err := f.media.Save(bg, org, bytes.NewReader(pngBytes(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(bg, `
		INSERT INTO appkit_websites (organization_id, settings, about_media_id)
		VALUES ($1, '{"schema":1,"mode":"site","about_text":"Kami menjahit sendiri.","services":[{"title":"Jahit","description":"Ukuran badan","icon":"wrench"}]}', $2)`,
		org, photo.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := f.pages.List(ctx); !l.Legacy.Available {
		t.Fatal("tidak ada tawaran impor padahal ada isi lama")
	}

	home, err := f.pages.ImportLegacy(ctx)
	if err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if home.Path != "/" || !strings.Contains(string(home.Draft), "Kami menjahit sendiri.") ||
		!strings.Contains(string(home.Draft), "Jahit") || !strings.Contains(string(home.Draft), photo.URL) {
		t.Errorf("beranda hasil impor = %+v", home)
	}
	if l, _ := f.pages.List(ctx); l.Legacy.Available {
		t.Error("tawaran impor masih ada sesudah diimpor")
	}
	if _, err := f.pages.ImportLegacy(ctx); kind(err) != appkit.KindNotFound {
		t.Errorf("impor kedua = %v", err)
	}
	// Isi lama di website tetap utuh.
	if legacy, err := f.sites.Legacy(bg, org); err != nil || legacy.AboutText == "" || legacy.AboutImage == nil {
		t.Errorf("isi lama sesudah impor = %+v, %v", legacy, err)
	}
}

func TestPermissionsAndTenancy(t *testing.T) {
	f := setup(t)
	a, b := uuid.New(), uuid.New()
	p := f.create(t, admin(a), "Layanan", "/layanan")
	in := input(p, doc(block("Hero", nil)))

	for name, tc := range map[string]struct {
		ctx  context.Context
		want error
	}{
		"tanpa izin": {staff(a), testdb.ErrDenied},
		"tanpa sesi": {context.Background(), testdb.ErrNoSession},
	} {
		check := func(op string, err error) {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s %s = %v", op, name, err)
			}
		}
		_, err := f.pages.List(tc.ctx)
		check("List", err)
		_, err = f.pages.Create(tc.ctx, pages.CreateInput{Title: "A", Path: "/a"})
		check("Create", err)
		_, err = f.pages.Get(tc.ctx, p.ID)
		check("Get", err)
		_, err = f.pages.Update(tc.ctx, p.ID, in)
		check("Update", err)
		_, err = f.pages.Publish(tc.ctx, p.ID, in)
		check("Publish", err)
		_, err = f.pages.Unpublish(tc.ctx, p.ID)
		check("Unpublish", err)
		check("Delete", f.pages.Delete(tc.ctx, p.ID))
		_, err = f.pages.Revisions(tc.ctx, p.ID)
		check("Revisions", err)
		_, err = f.pages.ImportLegacy(tc.ctx)
		check("ImportLegacy", err)
	}
	// Pemegang izin tanpa identitas pengguna tidak dapat menerbitkan.
	anonymous := testdb.With(context.Background(), testdb.Session{Organization: a, Permissions: []appkit.Permission{pages.Manage}})
	if _, err := f.pages.Publish(anonymous, p.ID, in); !errors.Is(err, testdb.ErrNoSession) {
		t.Errorf("Publish tanpa pengguna = %v", err)
	}

	// Halaman A tidak ada bagi organization B.
	ctxB := admin(b)
	if l, _ := f.pages.List(ctxB); len(l.Data) != 0 {
		t.Errorf("B melihat halaman A: %+v", l.Data)
	}
	for op, err := range map[string]error{
		"Get":       func() error { _, err := f.pages.Get(ctxB, p.ID); return err }(),
		"Update":    func() error { _, err := f.pages.Update(ctxB, p.ID, in); return err }(),
		"Publish":   func() error { _, err := f.pages.Publish(ctxB, p.ID, in); return err }(),
		"Unpublish": func() error { _, err := f.pages.Unpublish(ctxB, p.ID); return err }(),
		"Delete":    f.pages.Delete(ctxB, p.ID),
		"Revisions": func() error { _, err := f.pages.Revisions(ctxB, p.ID); return err }(),
		"Upload":    func() error { _, err := f.pages.UploadImage(ctxB, p.ID, bytes.NewReader(pngBytes(t))); return err }(),
	} {
		if kind(err) != appkit.KindNotFound {
			t.Errorf("%s lintas organization = %v, ingin tidak ditemukan", op, err)
		}
	}
	// Alamat yang sama boleh dipakai organization lain.
	f.create(t, ctxB, "Layanan", "/layanan")
	if got, _ := f.pages.Get(admin(a), p.ID); got.Version != 1 || got.Status != pages.StatusDraft {
		t.Errorf("halaman A berubah oleh B: %+v", got)
	}
}

func TestRoutes(t *testing.T) {
	f := setup(t)
	org := uuid.New()
	ctx := admin(org)
	mux := http.NewServeMux()
	appkit.Register(mux, "/v1", f.pages.Routes()...)
	do := func(c context.Context, method, path string, body []byte) (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(c))
		return rec.Code, rec.Body.String()
	}

	if code, _ := do(staff(org), http.MethodPost, "/v1/pages", []byte("bukan json")); code != http.StatusForbidden {
		t.Errorf("POST tanpa izin = %d", code)
	}
	if code, _ := do(context.Background(), http.MethodGet, "/v1/pages", nil); code != http.StatusUnauthorized {
		t.Errorf("GET tanpa sesi = %d", code)
	}
	code, raw := do(ctx, http.MethodPost, "/v1/pages", []byte(`{"title":"Layanan","path":"/layanan"}`))
	var p pages.Page
	if err := json.Unmarshal([]byte(raw), &p); err != nil || code != http.StatusCreated {
		t.Fatalf("POST /pages = %d, %s", code, raw)
	}
	base := "/v1/pages/" + p.ID.String()
	if code, raw := do(ctx, http.MethodPost, "/v1/pages", []byte(`{"title":"A","alamat":"/a"}`)); code != http.StatusBadRequest || !strings.Contains(raw, `"alamat"`) {
		t.Errorf("field tak dikenal = %d, %s", code, raw)
	}
	if code, raw := do(ctx, http.MethodGet, base, nil); code != http.StatusOK || !strings.Contains(raw, `"draft":`) {
		t.Errorf("GET satu halaman = %d, %s", code, raw)
	}
	body, _ := json.Marshal(input(p, doc(block("Hero", map[string]any{"title": "Halo"}))))
	if code, raw := do(ctx, http.MethodPut, base, body); code != http.StatusOK || !strings.Contains(raw, `"version":2`) {
		t.Errorf("PUT = %d, %s", code, raw)
	}
	if code, _ := do(ctx, http.MethodPut, base, body); code != http.StatusConflict {
		t.Errorf("PUT dengan version lama = %d", code)
	}
	p.Version = 2
	body, _ = json.Marshal(input(p, doc(block("Hero", map[string]any{"title": "Halo"}))))
	if code, raw := do(ctx, http.MethodPost, base+"/publish", body); code != http.StatusOK || !strings.Contains(raw, `"status":"published"`) {
		t.Errorf("POST publish = %d, %s", code, raw)
	}
	code, raw = do(ctx, http.MethodGet, base+"/revisions", nil)
	var revs struct {
		Data []pages.Revision `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &revs); err != nil || code != http.StatusOK || len(revs.Data) != 1 || !revs.Data[0].Live {
		t.Fatalf("GET revisions = %d, %s", code, raw)
	}
	if code, raw := do(ctx, http.MethodPost, base+"/revisions/"+revs.Data[0].ID.String()+"/restore", []byte(`{"version":3}`)); code != http.StatusOK {
		t.Errorf("POST restore = %d, %s", code, raw)
	}
	if code, _ := do(ctx, http.MethodPost, base+"/images", pngBytes(t)); code != http.StatusCreated {
		t.Errorf("POST images = %d", code)
	}
	if code, raw := do(ctx, http.MethodPut, base+"/images/seo", pngBytes(t)); code != http.StatusOK || !strings.Contains(raw, `"image":{`) {
		t.Errorf("PUT images/seo = %d, %s", code, raw)
	}
	if code, _ := do(ctx, http.MethodDelete, base+"/images/seo", nil); code != http.StatusOK {
		t.Errorf("DELETE images/seo = %d", code)
	}
	if code, raw := do(ctx, http.MethodPost, base+"/unpublish", nil); code != http.StatusOK || !strings.Contains(raw, `"status":"draft"`) {
		t.Errorf("POST unpublish = %d, %s", code, raw)
	}
	if code, _ := do(ctx, http.MethodGet, "/v1/pages/bukan-uuid", nil); code != http.StatusNotFound {
		t.Errorf("GET id salah bentuk = %d", code)
	}
	if code, _ := do(admin(uuid.New()), http.MethodGet, base, nil); code != http.StatusNotFound {
		t.Errorf("GET lintas organization = %d", code)
	}
	if code, _ := do(ctx, http.MethodPost, "/v1/pages/import-legacy", nil); code != http.StatusNotFound {
		t.Errorf("POST import-legacy tanpa isi lama = %d", code)
	}
	if code, raw := do(ctx, http.MethodDelete, base, nil); code != http.StatusNoContent || raw != "" {
		t.Errorf("DELETE = %d, %q", code, raw)
	}
}

func TestNewRejectsInvalidSetup(t *testing.T) {
	f := setup(t)
	base := func() pages.Options {
		return pages.Options{Blocks: catalog(), Limit: func(context.Context, uuid.UUID) (int64, error) { return pages.Unlimited, nil }}
	}
	noUser := testdb.Hooks()
	noUser.User = nil
	for name, build := range map[string]func() (*pages.Service, error){
		"tanpa pool": func() (*pages.Service, error) {
			return pages.New(nil, f.sites, f.media, f.trail, testdb.Hooks(), base())
		},
		"tanpa website": func() (*pages.Service, error) {
			return pages.New(f.pool, nil, f.media, f.trail, testdb.Hooks(), base())
		},
		"tanpa media": func() (*pages.Service, error) {
			return pages.New(f.pool, f.sites, nil, f.trail, testdb.Hooks(), base())
		},
		"tanpa jejak audit": func() (*pages.Service, error) {
			return pages.New(f.pool, f.sites, f.media, nil, testdb.Hooks(), base())
		},
		"tanpa Hooks.User": func() (*pages.Service, error) { return pages.New(f.pool, f.sites, f.media, f.trail, noUser, base()) },
	} {
		if _, err := build(); err == nil {
			t.Errorf("New %s lolos", name)
		}
	}
	for name, change := range map[string]func(*pages.Options){
		"tanpa Limit":             func(o *pages.Options) { o.Limit = nil },
		"tanpa blok":              func(o *pages.Options) { o.Blocks = nil },
		"jenis blok kembar":       func(o *pages.Options) { o.Blocks = append(o.Blocks, pages.Block{Type: "Hero"}) },
		"jenis blok tidak sah":    func(o *pages.Options) { o.Blocks = append(o.Blocks, pages.Block{Type: "hero-besar"}) },
		"isian bernama id":        func(o *pages.Options) { o.Blocks[0].Fields["id"] = pages.Field{Kind: pages.FieldText} },
		"jenis isian tak dikenal": func(o *pages.Options) { o.Blocks[0].Fields["x"] = pages.Field{Kind: "html"} },
		"daftar tanpa isian":      func(o *pages.Options) { o.Blocks[0].Fields["x"] = pages.Field{Kind: pages.FieldList} },
		"teks dengan isian": func(o *pages.Options) {
			o.Blocks[0].Fields["x"] = pages.Field{Kind: pages.FieldText, Fields: map[string]pages.Field{"a": {Kind: pages.FieldText}}}
		},
		"alamat terlarang tidak sah": func(o *pages.Options) { o.Reserved = []string{"/"} },
	} {
		t.Run(name, func(t *testing.T) {
			o := base()
			o.Blocks = catalog()
			change(&o)
			if _, err := pages.New(f.pool, f.sites, f.media, f.trail, testdb.Hooks(), o); err == nil {
				t.Error("susunan yang tidak sah lolos")
			}
		})
	}
}
