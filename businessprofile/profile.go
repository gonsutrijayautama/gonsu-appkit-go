// Package businessprofile adalah profil bisnis sebuah organization di dalam
// produk: nama, bidang usaha, kontak, identitas legal, alamat, dan logo.
//
// Ini sumber identitas tenant untuk seluruh aplikasi — kepala aplikasi,
// halaman depan, dan dokumen yang dicetak modul lain. Profil ini milik
// produk dan diisi di produk; ia tidak ditarik dari platform GONSU One.
// Makna kolomnya disamakan dengan profil penagihan platform supaya menyalin
// dari sana kelak tidak butuh penerjemahan.
//
// Aturan aksesnya:
//
//   - membaca: setiap pengguna yang sudah login di organization itu;
//   - mengubah profil dan logo: pemegang izin Manage.
//
// organization selalu datang dari Hooks.Organization, tidak pernah dari
// request.
package businessprofile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	wilayah "github.com/aliziodev/go-indonesia-regions"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/audit"
	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
)

// Manage adalah izin mengubah profil bisnis dan logonya.
const Manage appkit.Permission = "settings.business.manage"

// Service mengelola profil bisnis.
type Service struct {
	pool  *pgxpool.Pool
	media *media.Service
	trail *audit.Service
	hooks appkit.Hooks
}

// New mengembalikan service profil bisnis. Logo disimpan lewat m, dan setiap
// perubahan dicatat lewat trail.
func New(pool *pgxpool.Pool, m *media.Service, trail *audit.Service, hooks appkit.Hooks) (*Service, error) {
	switch {
	case pool == nil:
		return nil, errors.New("businessprofile: pool wajib diisi")
	case m == nil:
		return nil, errors.New("businessprofile: service media wajib diisi")
	case trail == nil:
		return nil, errors.New("businessprofile: service jejak audit wajib diisi")
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	return &Service{pool: pool, media: m, trail: trail, hooks: hooks}, nil
}

// Nama tindakan di jejak audit, kelompok audit.CategorySettings.
const (
	// ActionUpdated: isian profil disimpan. Details "fields" menyebut isian
	// yang berubah, tanpa nilainya.
	ActionUpdated     = "business_profile.updated"
	ActionLogoChanged = "business_profile.logo_changed"
	ActionLogoRemoved = "business_profile.logo_removed"
)

// Profile adalah profil bisnis di API. Isian yang belum diisi berupa string
// kosong, bukan null.
type Profile struct {
	// DisplayName kosong berarti profil belum pernah disimpan.
	DisplayName string `json:"display_name"`
	Industry    string `json:"industry"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	// BusinessType: "", "individual", atau "company".
	BusinessType string `json:"business_type"`
	LegalName    string `json:"legal_name"`
	// TaxID adalah NPWP 16 digit tanpa pemisah.
	TaxID string `json:"tax_id"`
	// Address adalah nama jalan, nomor, RT/RW — tanpa wilayah.
	Address string `json:"address"`
	// RegionCode adalah kode Kemendagri, minimal kabupaten/kota.
	RegionCode string `json:"region_code"`
	// Region diturunkan dari RegionCode saat dibaca; null bila kosong atau
	// kodenya sudah tidak ada di data wilayah.
	Region   *Region `json:"region"`
	Postcode string  `json:"postcode"`
	// AddressText adalah alamat lengkap satu baris: Address, wilayah, kode pos.
	AddressText string      `json:"address_text"`
	Logo        *media.File `json:"logo"`
	// Version dikirim balik saat menyimpan (Input.Version): simpan yang
	// membawa version lama ditolak. 0 bila belum pernah disimpan.
	Version int `json:"version"`
	// UpdatedAt null bila belum ada yang pernah disimpan.
	UpdatedAt *time.Time `json:"updated_at"`
}

// Region adalah wilayah yang sudah diuraikan sampai provinsi.
type Region struct {
	Province Named  `json:"province"`
	Regency  Named  `json:"regency"`
	District *Named `json:"district"`
	Village  *Named `json:"village"`
	// Label: "Desa Pasteur, Kecamatan Sukajadi, Kota Bandung, Jawa Barat".
	Label string `json:"label"`
}

// Named adalah kode dan nama resmi satu wilayah.
type Named struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func resolveRegion(code string) *Region {
	if code == "" {
		return nil
	}
	addr, ok := wilayah.Resolve(code)
	if !ok || addr.Province == nil || addr.Regency == nil {
		return nil
	}
	r := &Region{
		Province: Named{Code: addr.Province.Code, Name: addr.Province.Name},
		Regency:  Named{Code: addr.Regency.Code, Name: addr.Regency.Name},
		Label:    addr.Format(wilayah.WithoutPostalCode()),
	}
	if addr.District != nil {
		r.District = &Named{Code: addr.District.Code, Name: addr.District.Name}
	}
	if addr.Village != nil {
		r.Village = &Named{Code: addr.Village.Code, Name: addr.Village.Name}
	}
	return r
}

func addressText(address string, region *Region, postcode string) string {
	var parts []string
	// Alamat boleh beberapa baris; di sini dijadikan satu baris.
	for line := range strings.SplitSeq(address, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	if region != nil {
		parts = append(parts, region.Label)
	}
	text := strings.Join(parts, ", ")
	if postcode != "" {
		text = strings.TrimSpace(text + " " + postcode)
	}
	return text
}

// Get membaca profil organization request ini. Setiap pengguna yang sudah
// login boleh membacanya. Profil yang belum pernah disimpan dikembalikan
// kosong, bukan galat.
func (s *Service) Get(ctx context.Context) (Profile, error) {
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Profile{}, err
	}
	return s.Lookup(ctx, org)
}

// Lookup membaca profil org tanpa sesi, untuk dipakai kode server: halaman
// publik, dokumen yang dicetak modul lain. Jangan menyambungkannya ke
// organization yang datang dari request.
func (s *Service) Lookup(ctx context.Context, org uuid.UUID) (Profile, error) {
	var (
		p    Profile
		logo *uuid.UUID
		at   time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT display_name, industry, email, phone, business_type, legal_name, tax_id,
		       address, region_code, postcode, logo_media_id, version, updated_at
		FROM appkit_business_profiles
		WHERE organization_id = $1`, org).Scan(
		&p.DisplayName, &p.Industry, &p.Email, &p.Phone, &p.BusinessType, &p.LegalName, &p.TaxID,
		&p.Address, &p.RegionCode, &p.Postcode, &logo, &p.Version, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, nil
	}
	if err != nil {
		return Profile{}, fmt.Errorf("businessprofile: membaca profil: %w", err)
	}
	p.UpdatedAt = &at
	p.Region = resolveRegion(p.RegionCode)
	p.AddressText = addressText(p.Address, p.Region, p.Postcode)
	if logo != nil {
		f, err := s.media.Get(ctx, org, *logo)
		if err != nil {
			return Profile{}, fmt.Errorf("businessprofile: membaca logo: %w", err)
		}
		p.Logo = &f
	}
	return p, nil
}

// errChanged: profil sudah disimpan orang lain sejak dibaca.
var errChanged = appkit.Conflict("Profil bisnis sudah diubah orang lain. Muat ulang halaman, lalu ulangi perubahan Anda.")

// Update menyimpan seluruh isian profil (bukan sebagian): isian yang dikirim
// kosong menjadi kosong. Logo tidak ikut berubah.
//
// in.Version harus sama dengan Version profil yang dibaca; bila profil sudah
// berubah sejak itu, simpan ditolak dengan galat KindConflict.
func (s *Service) Update(ctx context.Context, in Input) (Profile, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return Profile{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Profile{}, err
	}
	if errs := in.normalize(); len(errs) > 0 {
		return Profile{}, appkit.Validation("Isian belum lengkap.", errs...)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Profile{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Isi sekarang dibaca dan dikunci, untuk mencatat isian mana yang berubah.
	// Profil yang belum pernah disimpan dibandingkan dengan isian kosong.
	var old Input
	err = tx.QueryRow(ctx, `
		SELECT display_name, industry, email, phone, business_type, legal_name, tax_id,
		       address, region_code, postcode
		FROM appkit_business_profiles
		WHERE organization_id = $1 FOR UPDATE`, org).Scan(
		&old.DisplayName, &old.Industry, &old.Email, &old.Phone, &old.BusinessType, &old.LegalName, &old.TaxID,
		&old.Address, &old.RegionCode, &old.Postcode)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("businessprofile: membaca profil: %w", err)
	}

	// Baris baru hanya untuk version 0; baris yang ada hanya berubah bila
	// version-nya masih yang dibaca pengirim. Tidak ada baris yang kembali
	// berarti salah satunya tidak terpenuhi.
	var version int
	err = tx.QueryRow(ctx, `
		INSERT INTO appkit_business_profiles (organization_id, display_name, industry, email, phone,
			business_type, legal_name, tax_id, address, region_code, postcode, version)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 1 WHERE $12 = 0
		ON CONFLICT (organization_id) DO UPDATE SET
			display_name = EXCLUDED.display_name, industry = EXCLUDED.industry,
			email = EXCLUDED.email, phone = EXCLUDED.phone,
			business_type = EXCLUDED.business_type, legal_name = EXCLUDED.legal_name,
			tax_id = EXCLUDED.tax_id, address = EXCLUDED.address,
			region_code = EXCLUDED.region_code, postcode = EXCLUDED.postcode,
			version = appkit_business_profiles.version + 1, updated_at = now()
		WHERE appkit_business_profiles.version = 0
		RETURNING version`,
		org, in.DisplayName, in.Industry, in.Email, in.Phone,
		in.BusinessType, in.LegalName, in.TaxID, in.Address, in.RegionCode, in.Postcode, in.Version).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) && in.Version > 0 {
		err = tx.QueryRow(ctx, `
			UPDATE appkit_business_profiles SET
				display_name = $2, industry = $3, email = $4, phone = $5,
				business_type = $6, legal_name = $7, tax_id = $8, address = $9,
				region_code = $10, postcode = $11,
				version = version + 1, updated_at = now()
			WHERE organization_id = $1 AND version = $12
			RETURNING version`,
			org, in.DisplayName, in.Industry, in.Email, in.Phone,
			in.BusinessType, in.LegalName, in.TaxID, in.Address, in.RegionCode, in.Postcode, in.Version).Scan(&version)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, errChanged
	}
	if err != nil {
		return Profile{}, fmt.Errorf("businessprofile: menyimpan profil: %w", err)
	}
	// Simpan tanpa perubahan tidak dicatat: tidak ada yang diubah.
	if fields := in.changed(old); len(fields) > 0 {
		if err := s.trail.RecordTx(ctx, tx, audit.Entry{
			Category: audit.CategorySettings, Action: ActionUpdated,
			Summary: "Profil bisnis diubah.", Details: map[string]any{"fields": fields},
		}); err != nil {
			return Profile{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Profile{}, err
	}
	return s.Lookup(ctx, org)
}

// SetLogo mengganti logo dengan gambar dari r. Logo lama dihapus.
func (s *Service) SetLogo(ctx context.Context, r io.Reader) (Profile, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return Profile{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Profile{}, err
	}
	// Logo yang sekarang akan dihapus swapLogo, jadi tidak dihitung ke kuota.
	var current *uuid.UUID
	err = s.pool.QueryRow(ctx, `
		SELECT logo_media_id FROM appkit_business_profiles WHERE organization_id = $1`, org).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("businessprofile: membaca logo: %w", err)
	}
	f, err := s.media.SaveReplacing(ctx, org, r, current)
	if err != nil {
		return Profile{}, err
	}
	if err := s.swapLogo(ctx, org, &f.ID); err != nil {
		// Berkas baru belum dirujuk siapa pun; jangan ditinggal menjadi sampah.
		_ = s.media.Delete(context.WithoutCancel(ctx), org, f.ID)
		return Profile{}, err
	}
	return s.Lookup(ctx, org)
}

// RemoveLogo menghapus logo. Profil tanpa logo bukan galat.
func (s *Service) RemoveLogo(ctx context.Context) (Profile, error) {
	if err := s.hooks.Authorize(ctx, Manage); err != nil {
		return Profile{}, err
	}
	org, err := s.hooks.Organization(ctx)
	if err != nil {
		return Profile{}, err
	}
	if err := s.swapLogo(ctx, org, nil); err != nil {
		return Profile{}, err
	}
	return s.Lookup(ctx, org)
}

// swapLogo memasang logo baru (nil: tanpa logo) lalu menghapus berkas logo
// lama. Baris profil dikunci selama pertukaran, supaya dua unggahan
// bersamaan tidak meninggalkan berkas yang tidak dirujuk siapa pun.
func (s *Service) swapLogo(ctx context.Context, org uuid.UUID, logo *uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Logo boleh diunggah sebelum profil pertama kali disimpan.
	if _, err := tx.Exec(ctx, `
		INSERT INTO appkit_business_profiles (organization_id) VALUES ($1)
		ON CONFLICT (organization_id) DO NOTHING`, org); err != nil {
		return fmt.Errorf("businessprofile: menyiapkan profil: %w", err)
	}
	var old *uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT logo_media_id FROM appkit_business_profiles
		WHERE organization_id = $1 FOR UPDATE`, org).Scan(&old); err != nil {
		return fmt.Errorf("businessprofile: mengunci profil: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE appkit_business_profiles SET logo_media_id = $2, updated_at = now()
		WHERE organization_id = $1`, org, logo); err != nil {
		return fmt.Errorf("businessprofile: memasang logo: %w", err)
	}
	// Menghapus logo yang tidak ada tidak mengubah apa pun, jadi tidak dicatat.
	entry := audit.Entry{Category: audit.CategorySettings, Action: ActionLogoChanged, Summary: "Logo bisnis diganti."}
	if logo == nil {
		entry.Action, entry.Summary = ActionLogoRemoved, "Logo bisnis dihapus."
	}
	if logo != nil || old != nil {
		if err := s.trail.RecordTx(ctx, tx, entry); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if old != nil {
		// Logo sudah berganti; gagal menghapus berkas lama tidak membatalkannya.
		_ = s.media.Delete(context.WithoutCancel(ctx), org, *old)
	}
	return nil
}
