package pages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gonsutrijayautama/gonsu-appkit-go/website"
)

// Public adalah halaman terbit untuk pengunjung tanpa akun. Draf tidak
// pernah ada di sini.
type Public struct {
	Path  string    `json:"path"`
	Title string    `json:"title"`
	SEO   PublicSEO `json:"seo"`
	// Data adalah data penyusun halaman versi TERBIT.
	Data json.RawMessage `json:"data"`
	// Navigation adalah halaman terbit yang tampil di menu, urut.
	Navigation []NavItem `json:"navigation"`
}

// PublicSEO adalah judul dan pratinjau tautan halaman, sudah dengan
// bawaannya.
type PublicSEO struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// ImageURL relatif terhadap akar situs; kosong bila tidak ada gambar
	// halaman maupun gambar website.
	ImageURL string `json:"image_url"`
}

// NavItem adalah satu tautan menu situs.
type NavItem struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

// Lookup mengembalikan halaman terbit org di alamat path, TANPA sesi: untuk
// penyaji halaman publik produk. org ditentukan kode server dari alamat
// permintaan (website.Service.PublicOrganization), tidak pernah dari body
// atau query. ok false — bukan galat — bila tidak ada halaman terbit di sana,
// atau mode website organization itu "signin".
//
// site adalah tampilan publik website organization itu: identitasnya untuk
// kepala dan kaki halaman.
func (s *Service) Lookup(ctx context.Context, org uuid.UUID, path string) (page Public, site website.Public, ok bool, err error) {
	if path != HomePath {
		path = strings.TrimSuffix(path, "/")
	}
	site, err = s.sites.Public(ctx, org)
	if err != nil {
		return Public{}, website.Public{}, false, err
	}
	// Mode "signin" adalah keputusan organization untuk tidak punya halaman
	// publik: halaman terbit pun tidak tampil.
	if site.Mode != website.ModeSite {
		return Public{}, site, false, nil
	}
	var (
		data              []byte
		seoTitle, seoDesc string
		seoMedia          *uuid.UUID
	)
	err = s.pool.QueryRow(ctx, `
		SELECT path, title, seo_title, seo_description, seo_media_id, published
		FROM appkit_pages
		WHERE organization_id = $1 AND path = $2 AND published IS NOT NULL`, org, path).
		Scan(&page.Path, &page.Title, &seoTitle, &seoDesc, &seoMedia, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return Public{}, site, false, nil
	}
	if err != nil {
		return Public{}, website.Public{}, false, fmt.Errorf("pages: membaca halaman: %w", err)
	}
	page.Data = data

	// Yang kosong diturunkan: judul dari judul halaman dan nama bisnis,
	// deskripsi dan gambar dari website.
	page.SEO = PublicSEO{Title: seoTitle, Description: seoDesc, ImageURL: site.SEO.ImageURL}
	if page.SEO.Title == "" {
		switch {
		case page.Path == HomePath && site.SEO.Title != "":
			page.SEO.Title = site.SEO.Title
		case page.Path != HomePath && site.Name != "":
			page.SEO.Title = page.Title + " — " + site.Name
		default:
			page.SEO.Title = page.Title
		}
	}
	if page.SEO.Description == "" {
		page.SEO.Description = site.SEO.Description
	}
	if seoMedia != nil {
		if f, err := s.files.Get(ctx, org, *seoMedia); err == nil {
			page.SEO.ImageURL = f.URL
		}
	}

	page.Navigation = []NavItem{}
	rows, err := s.pool.Query(ctx, `
		SELECT path, title FROM appkit_pages
		WHERE organization_id = $1 AND published IS NOT NULL AND nav_visible
		ORDER BY nav_position, path`, org)
	if err != nil {
		return Public{}, website.Public{}, false, fmt.Errorf("pages: membaca menu: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item NavItem
		if err := rows.Scan(&item.Path, &item.Title); err != nil {
			return Public{}, website.Public{}, false, fmt.Errorf("pages: membaca menu: %w", err)
		}
		page.Navigation = append(page.Navigation, item)
	}
	if err := rows.Err(); err != nil {
		return Public{}, website.Public{}, false, fmt.Errorf("pages: membaca menu: %w", err)
	}
	return page, site, true, nil
}

// DataScriptID adalah id elemen <script type="application/json"> yang membawa
// Public di kerangka halaman, di samping website.DataScriptID.
const DataScriptID = "gonsu-page"

var reHeadEnd = regexp.MustCompile(`(?i)</head>`)

// RenderPage mengembalikan kerangka halaman shell — hasil build frontend
// khusus halaman publik — dengan halaman terbit di alamat permintaan r
// tersisip: judul, SEO, tag pratinjau tautan, identitas website
// (website.DataScriptID), dan halamannya sendiri (DataScriptID).
//
// ok false berarti tidak ada halaman untuk ditampilkan di alamat itu: produk
// menyajikan 404-nya, atau — untuk "/" — halaman depan biasa lewat
// website.RenderHome. Kegagalan membaca data dicatat dan dijawab ok false
// juga: halaman publik tidak pernah menjadi galat 500 karena modul ini.
func (s *Service) RenderPage(r *http.Request, shell []byte) (out []byte, ok bool) {
	ctx := r.Context()
	org, err := s.sites.PublicOrganization(r)
	if err != nil {
		s.opts.Logger.WarnContext(ctx, "pages: organization halaman publik tidak diketahui", "error", err.Error())
		return nil, false
	}
	page, site, found, err := s.Lookup(ctx, org, r.URL.Path)
	if err != nil {
		s.opts.Logger.WarnContext(ctx, "pages: halaman publik tidak terbaca", "organization_id", org.String(), "path", r.URL.Path, "error", err.Error())
		return nil, false
	}
	if !found {
		return nil, false
	}
	return Inject(shell, page, site, website.RequestOrigin(r)), true
}

// Inject menyisipkan page dan identitas site ke kerangka halaman shell.
// Halaman tanpa </head> dikembalikan apa adanya. origin seperti di
// website.InjectHTML.
func Inject(shell []byte, page Public, site website.Public, origin string) []byte {
	if !reHeadEnd.Match(shell) {
		return shell
	}
	site.SEO = website.PublicSEO{Title: page.SEO.Title, Description: page.SEO.Description, ImageURL: page.SEO.ImageURL}
	out := website.InjectHTML(shell, site, origin)
	// json.Marshal meng-escape <, >, dan &, sehingga isi apa pun tidak dapat
	// menutup elemen script ini.
	data, err := json.Marshal(page)
	if err != nil {
		return out
	}
	loc := reHeadEnd.FindIndex(out)
	if loc == nil {
		return out
	}
	var b bytes.Buffer
	b.Grow(len(out) + len(data) + 80)
	b.Write(out[:loc[0]])
	b.WriteString(`<script id="` + DataScriptID + `" type="application/json">`)
	b.Write(data)
	b.WriteString(`</script>`)
	b.Write(out[loc[0]:])
	return b.Bytes()
}
