package website

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// Icons adalah nama ikon yang boleh dipakai sebuah layanan. Daftarnya tetap
// dan sama di setiap produk: frontend memetakan nama ini ke gambarnya, jadi
// nama yang tidak dikenal berarti layanan tanpa ikon. Daftar ini hanya
// bertambah.
var Icons = []string{
	"package", "chart", "wrench", "headset", "truck", "shield",
	"star", "users", "store", "clock", "wallet", "sparkles",
}

// Input adalah body PUT /website. Seluruh isian dikirim setiap kali; yang
// tidak dikirim dianggap kosong. Gambar tidak diatur lewat sini.
type Input struct {
	Mode    string `json:"mode"`
	Tagline string `json:"tagline"`
	Summary string `json:"summary"`
	About   struct {
		Text string `json:"text"`
	} `json:"about"`
	Services []Item   `json:"services"`
	Contact  Contact  `json:"contact"`
	Channels Channels `json:"channels"`
	SEO      struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"seo"`
	// Version adalah Settings.Version yang dibaca sebelum mengubah; 0 untuk
	// pengaturan yang belum pernah disimpan.
	Version int `json:"version"`
}

func (in Input) stored() stored {
	return stored{
		Schema: documentSchema, Mode: in.Mode, Tagline: in.Tagline, Summary: in.Summary,
		AboutText: in.About.Text, Services: in.Services, Contact: in.Contact, Channels: in.Channels,
		SEOTitle: in.SEO.Title, SEODescription: in.SEO.Description,
	}
}

// Batas isi.
const (
	maxTagline         = 120
	maxSummary         = 400
	maxAbout           = 2000
	maxServices        = 8
	maxServiceTitle    = 60
	maxServiceText     = 200
	maxHours           = 120
	maxURL             = 300
	maxSEOTitle        = 70
	maxSEODescription  = 160
	minWhatsAppDigits  = 8
	maxWhatsAppDigits  = 15
	indonesiaDialCode  = "62"
	whatsAppLinkPrefix = "https://wa.me/"
)

// channelHosts: alamat sebuah kanal harus berada di situs kanal itu. Tanpa
// ini isian "Instagram" dapat diisi tautan ke mana saja, dan halaman depan
// menampilkannya dengan ikon Instagram.
var channelHosts = map[string][]string{
	"instagram": {"instagram.com"},
	"facebook":  {"facebook.com", "fb.com"},
	"tiktok":    {"tiktok.com"},
	"youtube":   {"youtube.com", "youtu.be"},
	"linkedin":  {"linkedin.com"},
}

// normalize merapikan isian di tempat dan mengembalikan yang tidak sah. Yang
// disimpan adalah hasil akhirnya, bukan yang diketik.
func (in *Input) normalize() []appkit.FieldError {
	var errs []appkit.FieldError
	fail := func(field, message string) {
		errs = append(errs, appkit.FieldError{Field: field, Message: message})
	}
	text := func(field, label string, value *string, max int) {
		*value = strings.TrimSpace(strings.ReplaceAll(*value, "\r\n", "\n"))
		if utf8.RuneCountInString(*value) > max {
			fail(field, fmt.Sprintf("%s maksimal %d karakter.", label, max))
		}
	}

	if in.Version < 0 {
		fail("version", "Versi tidak sah.")
	}

	in.Mode = strings.TrimSpace(in.Mode)
	switch in.Mode {
	case "":
		in.Mode = ModeSignIn
	case ModeSignIn, ModeSite:
	default:
		fail("mode", "Pilih hanya pintu masuk atau web perusahaan.")
	}

	text("tagline", "Tagline", &in.Tagline, maxTagline)
	text("summary", "Ringkasan", &in.Summary, maxSummary)
	text("about.text", "Tentang kami", &in.About.Text, maxAbout)

	if in.Services == nil {
		in.Services = []Item{}
	}
	if len(in.Services) > maxServices {
		fail("services", fmt.Sprintf("Layanan maksimal %d.", maxServices))
	}
	for i := range in.Services {
		item := &in.Services[i]
		field := fmt.Sprintf("services[%d]", i)
		text(field+".title", "Nama layanan", &item.Title, maxServiceTitle)
		if item.Title == "" {
			fail(field+".title", "Nama layanan wajib diisi.")
		}
		text(field+".description", "Keterangan layanan", &item.Description, maxServiceText)
		item.Icon = strings.TrimSpace(item.Icon)
		if !slices.Contains(Icons, item.Icon) {
			fail(field+".icon", "Pilih ikon dari daftar.")
		}
	}

	text("contact.hours", "Jam kerja", &in.Contact.Hours, maxHours)
	in.Contact.MapURL = strings.TrimSpace(in.Contact.MapURL)
	if in.Contact.MapURL != "" && !validURL(in.Contact.MapURL, nil) {
		fail("contact.map_url", "Tautan peta harus alamat https yang lengkap.")
	}

	in.Channels.WhatsApp = strings.TrimSpace(in.Channels.WhatsApp)
	if in.Channels.WhatsApp != "" {
		number, ok := whatsAppNumber(in.Channels.WhatsApp)
		if !ok {
			fail("channels.whatsapp", "Nomor WhatsApp belum benar.")
		} else {
			in.Channels.WhatsApp = number
		}
	}
	for _, channel := range []struct {
		name  string
		value *string
	}{
		{"instagram", &in.Channels.Instagram}, {"facebook", &in.Channels.Facebook},
		{"tiktok", &in.Channels.TikTok}, {"youtube", &in.Channels.YouTube}, {"linkedin", &in.Channels.LinkedIn},
	} {
		*channel.value = strings.TrimSpace(*channel.value)
		hosts := channelHosts[channel.name]
		if *channel.value != "" && !validURL(*channel.value, hosts) {
			fail("channels."+channel.name, fmt.Sprintf("Isi alamat lengkapnya, misalnya https://%s/namaakun.", hosts[0]))
		}
	}

	text("seo.title", "Judul", &in.SEO.Title, maxSEOTitle)
	text("seo.description", "Deskripsi", &in.SEO.Description, maxSEODescription)

	return errs
}

// validURL: alamat https lengkap, dan — bila hosts diisi — berada di salah
// satu situs itu atau subdomainnya. Hanya https: alamat ini menjadi tautan di
// halaman publik, dan skema lain (javascript:, data:) tidak boleh sampai ke
// sana.
func validURL(raw string, hosts []string) bool {
	if utf8.RuneCountInString(raw) > maxURL {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	if hosts == nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range hosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

// whatsAppNumber menormalkan nomor yang diketik bebas menjadi digit dengan
// kode negara, bentuk yang dipakai tautan wa.me. Nomor berawalan 0 dianggap
// nomor Indonesia.
func whatsAppNumber(raw string) (string, bool) {
	var digits strings.Builder
	for i, c := range raw {
		switch {
		case c >= '0' && c <= '9':
			digits.WriteRune(c)
		case c == '+' && i == 0, c == ' ', c == '-', c == '(', c == ')', c == '.':
		default:
			return "", false
		}
	}
	number := digits.String()
	if strings.HasPrefix(number, "0") {
		number = indonesiaDialCode + strings.TrimLeft(number, "0")
	}
	if len(number) < minWhatsAppDigits || len(number) > maxWhatsAppDigits {
		return "", false
	}
	return number, true
}
