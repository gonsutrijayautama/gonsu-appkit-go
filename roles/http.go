package roles

import (
	"net/http"
	"path"
	"strconv"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// Routes adalah endpoint role, relatif terhadap akar API produk (`/v1`).
// Pasang di balik middleware sesi; semuanya meminta izin Manage:
//
//	GET    /roles           role, katalog izin, dan batas role buatan (Listing)
//	POST   /roles           buat role buatan; menjawab 201 dan rolenya
//	PUT    /roles/{key}     simpan seluruh isian; menjawab rolenya
//	DELETE /roles/{key}     hapus; menjawab 204
//	GET    /role-events?limit=   catatan perubahan, terbaru dulu
func (s *Service) Routes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/roles", Handler: s.handleList},
		{Method: http.MethodPost, Path: "/roles", Handler: s.handleCreate},
		{Method: http.MethodPut, Path: "/roles/{key}", Handler: s.handleUpdate},
		{Method: http.MethodDelete, Path: "/roles/{key}", Handler: s.handleDelete},
		{Method: http.MethodGet, Path: "/role-events", Handler: s.handleEvents},
	}
}

func (s *Service) respond(w http.ResponseWriter, r *http.Request, status int, out any, err error) {
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, status, out)
}

// decode memeriksa izin SEBELUM body dibaca: yang tidak berhak tidak perlu
// tahu isiannya salah di mana. Create dan Update memeriksanya lagi.
func (s *Service) decode(w http.ResponseWriter, r *http.Request, in *Input) bool {
	if err := s.hooks.Authorize(r.Context(), Manage); err != nil {
		s.hooks.WriteError(w, r, err)
		return false
	}
	if err := httpjson.Decode(w, r, in); err != nil {
		s.hooks.WriteError(w, r, err)
		return false
	}
	return true
}

// keyOf membaca Key dari segmen terakhir path; router tidak dilibatkan.
func keyOf(r *http.Request) string { return path.Base(r.URL.Path) }

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := s.List(r.Context())
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.Create(r.Context(), in)
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.Update(r.Context(), keyOf(r), in)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Delete(r.Context(), keyOf(r)); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Nilai limit yang tidak valid jatuh ke bawaan: ini parameter tampilan.
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.Events(r.Context(), limit)
	s.respond(w, r, http.StatusOK, map[string]any{"data": events}, err)
}
