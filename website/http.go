package website

import (
	"net/http"
	"path"
	"strconv"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// PublicPath adalah alamat tampilan publik, relatif terhadap akar situs.
const PublicPath = "/site.json"

// Routes adalah endpoint pengaturan website, relatif terhadap akar API produk
// (`/v1`). Pasang di balik middleware sesi:
//
//	GET    /website                  pengaturan organization ini
//	PUT    /website                  simpan seluruh isian (izin Manage)
//	PUT    /website/images/{slot}    body: isi gambar; slot "about" atau "seo" (izin Manage)
//	DELETE /website/images/{slot}    hapus gambar (izin Manage)
//
// Keempatnya menjawab pengaturan yang sudah diperbarui.
func (s *Service) Routes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/website", Handler: s.handleGet},
		{Method: http.MethodPut, Path: "/website", Handler: s.handleUpdate},
		{Method: http.MethodPut, Path: "/website/images/{slot}", Handler: s.handleSetImage},
		{Method: http.MethodDelete, Path: "/website/images/{slot}", Handler: s.handleRemoveImage},
	}
}

// PublicRoutes adalah endpoint TANPA SESI, dipasang di akar situs:
//
//	GET /site.json    tampilan publik (Public)
//
// Data yang sama disisipkan RenderHome ke halaman depan; endpoint ini untuk
// frontend yang halamannya tidak disajikan server ini, misalnya saat
// pengembangan.
func (s *Service) PublicRoutes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: PublicPath, Handler: s.handlePublic},
	}
}

func (s *Service) respond(w http.ResponseWriter, r *http.Request, out Settings, err error) {
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	out, err := s.Get(r.Context())
	s.respond(w, r, out, err)
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
	out, err := s.Update(r.Context(), in)
	s.respond(w, r, out, err)
}

// slotOf membaca slot dari segmen terakhir path; router tidak dilibatkan.
func slotOf(r *http.Request) Slot { return Slot(path.Base(r.URL.Path)) }

func (s *Service) handleSetImage(w http.ResponseWriter, r *http.Request) {
	if err := s.hooks.Authorize(r.Context(), Manage); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	// Satu byte di atas batas, supaya media.Save yang menyebut batasnya.
	body := http.MaxBytesReader(w, r.Body, s.media.MaxBytes()+1)
	out, err := s.SetImage(r.Context(), slotOf(r), body)
	s.respond(w, r, out, err)
}

func (s *Service) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	out, err := s.RemoveImage(r.Context(), slotOf(r))
	s.respond(w, r, out, err)
}

func (s *Service) handlePublic(w http.ResponseWriter, r *http.Request) {
	org, err := s.opts.PublicOrganization(r)
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	site, err := s.Public(r.Context(), org)
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	// Seumur cache di server: perubahan tetap tampil dalam selang yang sama.
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(s.opts.CacheTTL.Seconds())))
	httpjson.Write(w, http.StatusOK, site)
}
