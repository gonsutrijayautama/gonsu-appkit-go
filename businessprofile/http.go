package businessprofile

import (
	"net/http"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// Routes adalah endpoint profil bisnis, relatif terhadap akar API produk
// (`/v1`). Pasang di balik middleware sesi:
//
//	GET    /business-profile        profil organization ini
//	PUT    /business-profile        simpan seluruh isian (izin Manage)
//	PUT    /business-profile/logo   body: isi gambar PNG, JPEG, atau WebP (izin Manage)
//	DELETE /business-profile/logo   hapus logo (izin Manage)
//
// Keempatnya menjawab profil yang sudah diperbarui.
func (s *Service) Routes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/business-profile", Handler: s.handleGet},
		{Method: http.MethodPut, Path: "/business-profile", Handler: s.handleUpdate},
		{Method: http.MethodPut, Path: "/business-profile/logo", Handler: s.handleSetLogo},
		{Method: http.MethodDelete, Path: "/business-profile/logo", Handler: s.handleRemoveLogo},
	}
}

func (s *Service) respond(w http.ResponseWriter, r *http.Request, p Profile, err error) {
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, p)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.Get(r.Context())
	s.respond(w, r, p, err)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	// Izin diperiksa sebelum body dibaca: yang tidak berhak tidak perlu tahu
	// isiannya salah di mana. Update memeriksanya lagi.
	if err := s.hooks.Authorize(r.Context(), Manage); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	var in Input
	if err := httpjson.Decode(w, r, &in); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	p, err := s.Update(r.Context(), in)
	s.respond(w, r, p, err)
}

func (s *Service) handleSetLogo(w http.ResponseWriter, r *http.Request) {
	if err := s.hooks.Authorize(r.Context(), Manage); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	// Satu byte di atas batas, supaya media.Save yang menyebut batasnya.
	body := http.MaxBytesReader(w, r.Body, s.media.MaxBytes()+1)
	p, err := s.SetLogo(r.Context(), body)
	s.respond(w, r, p, err)
}

func (s *Service) handleRemoveLogo(w http.ResponseWriter, r *http.Request) {
	p, err := s.RemoveLogo(r.Context())
	s.respond(w, r, p, err)
}
