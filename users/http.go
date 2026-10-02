package users

import (
	"net/http"
	"path"

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// Routes adalah endpoint layar Pengguna & Akses, relatif terhadap akar API
// produk (`/v1`). Pasang di balik middleware sesi; semuanya meminta izin
// Manage:
//
//	GET   /users        pengguna, batas pengguna, kesiapan pemberian akses, dan role (Listing)
//	POST  /users        beri akses lewat email; menjawab 201 (Invited)
//	PATCH /users/{id}   ubah role atau status; menjawab penggunanya
//
// Pasang di LUAR penjaga hak pakai produk: mencabut akses orang yang keluar
// tidak boleh terhalang langganan yang sedang tidak aktif.
func (s *Service) Routes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/users", Handler: s.handleList},
		{Method: http.MethodPost, Path: "/users", Handler: s.handleInvite},
		{Method: http.MethodPatch, Path: "/users/{id}", Handler: s.handleUpdate},
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
// tahu isiannya salah di mana. Invite dan Update memeriksanya lagi.
func (s *Service) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := s.hooks.Authorize(r.Context(), Manage); err != nil {
		s.hooks.WriteError(w, r, err)
		return false
	}
	if err := httpjson.Decode(w, r, dst); err != nil {
		s.hooks.WriteError(w, r, err)
		return false
	}
	return true
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := s.List(r.Context())
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleInvite(w http.ResponseWriter, r *http.Request) {
	var in InviteInput
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.Invite(r.Context(), in)
	// Jawabannya dapat membawa sandi sementara: tidak boleh disimpan cache
	// mana pun di antara server dan peramban.
	w.Header().Set("Cache-Control", "no-store")
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var in UpdateInput
	if !s.decode(w, r, &in) {
		return
	}
	// Id dibaca dari segmen terakhir path; router tidak dilibatkan.
	id, err := uuid.Parse(path.Base(r.URL.Path))
	if err != nil {
		s.hooks.WriteError(w, r, errNotFound)
		return
	}
	out, err := s.Update(r.Context(), id, in)
	s.respond(w, r, http.StatusOK, out, err)
}
