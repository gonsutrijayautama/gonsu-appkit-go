package businessprofile

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	wilayah "github.com/aliziodev/go-indonesia-regions"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// Input adalah body PUT /business-profile. Seluruh isian dikirim setiap kali;
// yang tidak dikirim dianggap kosong.
type Input struct {
	DisplayName  string `json:"display_name"`
	Industry     string `json:"industry"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	BusinessType string `json:"business_type"`
	LegalName    string `json:"legal_name"`
	TaxID        string `json:"tax_id"`
	Address      string `json:"address"`
	RegionCode   string `json:"region_code"`
	Postcode     string `json:"postcode"`
	// Version adalah Profile.Version yang dibaca sebelum mengubah; 0 untuk
	// profil yang belum pernah disimpan.
	Version int `json:"version"`
}

// changed menyebut isian yang berbeda dari old, dengan nama field JSON-nya.
// Hanya namanya: nilai seperti NPWP tidak ikut masuk jejak audit.
func (in Input) changed(old Input) []string {
	var out []string
	for _, f := range []struct{ name, now, was string }{
		{"display_name", in.DisplayName, old.DisplayName},
		{"industry", in.Industry, old.Industry},
		{"email", in.Email, old.Email},
		{"phone", in.Phone, old.Phone},
		{"business_type", in.BusinessType, old.BusinessType},
		{"legal_name", in.LegalName, old.LegalName},
		{"tax_id", in.TaxID, old.TaxID},
		{"address", in.Address, old.Address},
		{"region_code", in.RegionCode, old.RegionCode},
		{"postcode", in.Postcode, old.Postcode},
	} {
		if f.now != f.was {
			out = append(out, f.name)
		}
	}
	return out
}

// Jenis usaha, sama dengan business_type di platform.
const (
	TypeIndividual = "individual"
	TypeCompany    = "company"
)

// Batas isi, sama dengan constraint kolomnya.
const (
	maxDisplayName = 120
	maxIndustry    = 120
	maxEmail       = 320
	maxPhone       = 32
	minPhoneDigits = 5
	maxLegalName   = 200
	maxAddress     = 500
)

var (
	reEmail    = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	rePhone    = regexp.MustCompile(`^[0-9+() .-]+$`)
	reDigits16 = regexp.MustCompile(`^[0-9]{16}$`)
	rePostcode = regexp.MustCompile(`^[0-9]{5}$`)
	// Pemisah yang lazim diketik orang pada NPWP: 01.234.567.8-901.000.
	taxSeparators = strings.NewReplacer(".", "", "-", "", " ", "")
)

// normalize merapikan isian di tempat dan mengembalikan yang tidak sah. Yang
// disimpan adalah hasil akhirnya, bukan yang diketik.
func (in *Input) normalize() []appkit.FieldError {
	var errs []appkit.FieldError
	fail := func(field, message string) {
		errs = append(errs, appkit.FieldError{Field: field, Message: message})
	}
	tooLong := func(field, label, value string, max int) {
		if utf8.RuneCountInString(value) > max {
			fail(field, fmt.Sprintf("%s maksimal %d karakter.", label, max))
		}
	}

	if in.Version < 0 {
		fail("version", "Versi tidak sah.")
	}

	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if in.DisplayName == "" {
		fail("display_name", "Nama bisnis wajib diisi.")
	}
	tooLong("display_name", "Nama bisnis", in.DisplayName, maxDisplayName)

	in.Industry = strings.TrimSpace(in.Industry)
	tooLong("industry", "Bidang usaha", in.Industry, maxIndustry)

	in.Email = strings.TrimSpace(in.Email)
	if in.Email != "" && (utf8.RuneCountInString(in.Email) > maxEmail || !reEmail.MatchString(in.Email)) {
		fail("email", "Alamat email belum benar.")
	}

	in.Phone = strings.TrimSpace(in.Phone)
	if in.Phone != "" {
		digits := 0
		for _, c := range in.Phone {
			if c >= '0' && c <= '9' {
				digits++
			}
		}
		if len(in.Phone) > maxPhone || digits < minPhoneDigits || !rePhone.MatchString(in.Phone) {
			fail("phone", "Nomor telepon belum benar.")
		}
	}

	in.BusinessType = strings.TrimSpace(in.BusinessType)
	switch in.BusinessType {
	case "", TypeIndividual, TypeCompany:
	default:
		fail("business_type", "Pilih perorangan atau badan usaha.")
	}

	in.LegalName = strings.TrimSpace(in.LegalName)
	tooLong("legal_name", "Nama legal", in.LegalName, maxLegalName)

	// NPWP disimpan 16 digit tanpa pemisah. NPWP lama 15 digit mendapat
	// awalan 0, sama dengan aturan platform.
	in.TaxID = taxSeparators.Replace(strings.TrimSpace(in.TaxID))
	if len(in.TaxID) == 15 {
		in.TaxID = "0" + in.TaxID
	}
	if in.TaxID != "" && !reDigits16.MatchString(in.TaxID) {
		fail("tax_id", "NPWP harus 16 digit, atau 15 digit untuk format lama.")
	}

	in.Address = strings.TrimSpace(strings.ReplaceAll(in.Address, "\r\n", "\n"))
	tooLong("address", "Alamat", in.Address, maxAddress)

	in.RegionCode = strings.TrimSpace(in.RegionCode)
	var region wilayah.Address
	if in.RegionCode != "" {
		addr, ok := wilayah.Resolve(in.RegionCode)
		switch {
		case !ok:
			fail("region_code", "Pilih wilayah dari daftar.")
		case addr.Level() < wilayah.LevelRegency:
			fail("region_code", "Pilih wilayah minimal sampai kabupaten/kota.")
		default:
			region = addr
		}
	}

	in.Postcode = strings.TrimSpace(in.Postcode)
	if in.Postcode == "" {
		// Kode pos desa terpilih; kosong bila wilayah belum sampai desa.
		in.Postcode = region.PostalCode()
	} else if !rePostcode.MatchString(in.Postcode) {
		// Hanya bentuknya yang diperiksa. Kode pos yang tidak cocok dengan
		// wilayahnya tetap diterima: data kode pos ikut berubah setiap desa
		// dimekarkan, dan menolak kode pos yang sah lebih merugikan.
		fail("postcode", "Kode pos harus 5 angka.")
	}
	return errs
}
