package website

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Public adalah yang boleh dilihat pengunjung tanpa akun: gabungan profil
// bisnis dan pengaturan website yang sudah disaring.
//
// Yang TIDAK PERNAH ada di sini: NPWP, nama legal, dan jenis usaha. Pada
// ModeSignIn isinya hanya nama, logo, dan judul — organization itu memilih
// tidak punya web publik.
type Public struct {
	Mode string `json:"mode"`
	// Name kosong bila profil bisnis belum diisi; halaman memakai nama produk.
	Name     string `json:"name"`
	Industry string `json:"industry"`
	// LogoURL relatif terhadap akar situs; kosong bila belum ada logo.
	LogoURL  string        `json:"logo_url"`
	Tagline  string        `json:"tagline"`
	Summary  string        `json:"summary"`
	About    PublicAbout   `json:"about"`
	Services []Item        `json:"services"`
	Contact  PublicContact `json:"contact"`
	// Channels berisi TAUTAN siap pakai; WhatsApp sudah berupa https://wa.me/….
	Channels Channels  `json:"channels"`
	SEO      PublicSEO `json:"seo"`
}

// PublicAbout adalah bagian "Tentang kami" untuk pengunjung.
type PublicAbout struct {
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

// PublicContact adalah kontak untuk pengunjung.
type PublicContact struct {
	// Address adalah alamat lengkap satu baris; kosong bila disembunyikan.
	Address string `json:"address"`
	// City adalah nama kabupaten/kota, tetap tampil walau alamat disembunyikan.
	City   string `json:"city"`
	Email  string `json:"email"`
	Phone  string `json:"phone"`
	Hours  string `json:"hours"`
	MapURL string `json:"map_url"`
}

// PublicSEO adalah judul dan pratinjau tautan, sudah dengan bawaannya.
type PublicSEO struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// ImageURL relatif terhadap akar situs; kosong bila tidak ada gambar
	// maupun logo.
	ImageURL string `json:"image_url"`
}

// Public mengembalikan tampilan publik org, dari memori bila masih segar.
//
// Bila database tidak terjangkau, tampilan terakhir yang pernah dibaca tetap
// disajikan: halaman depan lebih baik sedikit usang daripada gagal.
func (s *Service) Public(ctx context.Context, org uuid.UUID) (Public, error) {
	s.mu.Lock()
	entry, known := s.cache[org]
	s.mu.Unlock()
	if known && time.Since(entry.at) < s.opts.CacheTTL {
		return entry.site, nil
	}
	site, err := s.public(ctx, org)
	if err != nil {
		if known {
			s.opts.Logger.WarnContext(ctx, "website: tampilan publik disajikan dari memori",
				"organization_id", org.String(), "error", err.Error())
			return entry.site, nil
		}
		return Public{}, err
	}
	s.mu.Lock()
	s.cache[org] = cached{site: site, at: time.Now()}
	s.mu.Unlock()
	return site, nil
}

// forget membuang tampilan publik org dari memori, sesudah pengaturannya
// berubah di proses ini.
func (s *Service) forget(org uuid.UUID) {
	s.mu.Lock()
	delete(s.cache, org)
	s.mu.Unlock()
}

func (s *Service) public(ctx context.Context, org uuid.UUID) (Public, error) {
	profile, err := s.profiles.Lookup(ctx, org)
	if err != nil {
		return Public{}, err
	}
	settings, err := s.lookup(ctx, org)
	if err != nil {
		return Public{}, err
	}

	site := Public{Mode: settings.Mode, Name: profile.DisplayName, Services: []Item{}}
	if profile.Logo != nil {
		site.LogoURL = profile.Logo.URL
	}
	site.SEO = PublicSEO{Title: profile.DisplayName, ImageURL: site.LogoURL}
	if settings.Mode != ModeSite {
		return site, nil
	}

	site.Industry = profile.Industry
	site.Tagline = settings.Tagline
	site.Summary = settings.Summary
	site.About.Text = settings.About.Text
	if settings.About.Image != nil {
		site.About.ImageURL = settings.About.Image.URL
	}
	site.Services = settings.Services
	site.Contact = PublicContact{
		Email: profile.Email, Phone: profile.Phone,
		Hours: settings.Contact.Hours, MapURL: settings.Contact.MapURL,
	}
	if profile.Region != nil {
		site.Contact.City = profile.Region.Regency.Name
	}
	if !settings.Contact.HideAddress {
		site.Contact.Address = profile.AddressText
	}
	site.Channels = settings.Channels
	if site.Channels.WhatsApp != "" {
		site.Channels.WhatsApp = whatsAppLinkPrefix + site.Channels.WhatsApp
	}

	// Judul dan deskripsi: yang diisi pengelola, atau diturunkan dari isi.
	site.SEO.Title = firstNonEmpty(settings.SEO.Title, joinNonEmpty(" — ", profile.DisplayName, settings.Tagline))
	site.SEO.Description = firstNonEmpty(settings.SEO.Description, settings.Summary, settings.Tagline)
	if settings.SEO.Image != nil {
		site.SEO.ImageURL = settings.SEO.Image.URL
	}
	return site, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func joinNonEmpty(sep string, values ...string) string {
	var kept []string
	for _, v := range values {
		if v != "" {
			kept = append(kept, v)
		}
	}
	return strings.Join(kept, sep)
}

// DataScriptID adalah id elemen <script type="application/json"> yang membawa
// Public di halaman depan. Frontend membacanya saat halaman dibuka, tanpa
// memanggil API.
const DataScriptID = "gonsu-site"

var (
	reTitle       = regexp.MustCompile(`(?is)<title>.*?</title>`)
	reDescription = regexp.MustCompile(`(?i)<meta\s+name="description"\s+content="[^"]*"\s*/?>`)
	reHeadEnd     = regexp.MustCompile(`(?i)</head>`)
)

// InjectHTML menyisipkan site ke halaman depan hasil build frontend: judul,
// deskripsi, tag pratinjau tautan, dan datanya sendiri sebagai JSON.
//
// Disisipkan di server, bukan diambil peramban lewat API, karena layanan yang
// membuat pratinjau tautan (WhatsApp, Facebook) tidak menjalankan JavaScript,
// dan supaya halaman tidak butuh satu permintaan lagi sebelum menampilkan
// identitas bisnisnya.
//
// origin adalah alamat aplikasi seperti yang dipakai pengunjung
// ("https://toko.example"), untuk gambar pratinjau yang wajib beralamat
// lengkap. Alamat itu dapat berganti, jadi selalu diturunkan dari permintaan,
// tidak pernah disimpan. Halaman tanpa </head> dikembalikan apa adanya.
func InjectHTML(page []byte, site Public, origin string) []byte {
	if !reHeadEnd.Match(page) {
		return page
	}
	title, description := site.SEO.Title, site.SEO.Description

	// Judul dan deskripsi bawaan build hanya diganti bila ada penggantinya.
	if title != "" {
		page = replaceFirst(page, reTitle, "<title>"+html.EscapeString(title)+"</title>")
	}
	var head strings.Builder
	if description != "" {
		tag := `<meta name="description" content="` + html.EscapeString(description) + `"/>`
		if reDescription.Match(page) {
			page = replaceFirst(page, reDescription, tag)
		} else {
			head.WriteString(tag)
		}
	}
	meta := func(property, content string) {
		if content != "" {
			head.WriteString(`<meta property="` + property + `" content="` + html.EscapeString(content) + `"/>`)
		}
	}
	meta("og:type", "website")
	meta("og:title", title)
	meta("og:description", description)
	if origin != "" {
		meta("og:url", origin+"/")
		if site.SEO.ImageURL != "" {
			meta("og:image", origin+site.SEO.ImageURL)
		}
	}

	// json.Marshal meng-escape <, >, dan & menjadi < dan seterusnya,
	// sehingga isi apa pun tidak dapat menutup elemen script ini.
	data, err := json.Marshal(site)
	if err == nil {
		head.WriteString(`<script id="` + DataScriptID + `" type="application/json">`)
		head.Write(data)
		head.WriteString(`</script>`)
	}
	head.WriteString("</head>")
	return replaceFirst(page, reHeadEnd, head.String())
}

// replaceFirst mengganti kecocokan PERTAMA re dengan repl, apa adanya — tanpa
// perluasan $1, yang akan menafsirkan tanda dolar di isian pengguna.
func replaceFirst(page []byte, re *regexp.Regexp, repl string) []byte {
	loc := re.FindIndex(page)
	if loc == nil {
		return page
	}
	var out bytes.Buffer
	out.Grow(len(page) + len(repl))
	out.Write(page[:loc[0]])
	out.WriteString(repl)
	out.Write(page[loc[1]:])
	return out.Bytes()
}

// RenderHome mengembalikan halaman depan untuk permintaan r: page dengan data
// organization-nya disisipkan.
//
// TIDAK PERNAH gagal. Halaman depan adalah probe platform dan pintu masuk
// aplikasi: bila organization atau datanya tidak dapat dibaca, page
// dikembalikan apa adanya dan sebabnya dicatat.
func (s *Service) RenderHome(r *http.Request, page []byte) []byte {
	org, err := s.opts.PublicOrganization(r)
	if err != nil {
		s.opts.Logger.WarnContext(r.Context(), "website: organization halaman depan tidak diketahui", "error", err.Error())
		return page
	}
	site, err := s.Public(r.Context(), org)
	if err != nil {
		s.opts.Logger.WarnContext(r.Context(), "website: halaman depan disajikan tanpa data",
			"organization_id", org.String(), "error", err.Error())
		return page
	}
	return InjectHTML(page, site, requestOrigin(r))
}

// requestOrigin menurunkan alamat aplikasi dari permintaan. Di belakang
// reverse proxy, skemanya dibaca dari X-Forwarded-Proto.
//
// Nilainya hanya dipakai untuk menyusun alamat gambar pratinjau di jawaban
// permintaan itu sendiri, jadi header yang dipalsukan hanya memengaruhi
// pengirimnya.
func requestOrigin(r *http.Request) string {
	if r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); strings.TrimSpace(forwarded) == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
