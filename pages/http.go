package pages

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// PublicPath adalah alamat isi halaman terbit untuk pengunjung, relatif
// terhadap akar situs.
const PublicPath = "/page.json"

// Routes adalah endpoint halaman, relatif terhadap akar API produk (`/v1`).
// Pasang di balik middleware sesi; semuanya meminta izin Manage:
//
//	GET    /pages                                      daftar, batas paket, tawaran impor (Listing)
//	POST   /pages                                      buat halaman kosong; menjawab 201
//	POST   /pages/import-legacy                        isi website lama ke draf beranda
//	GET    /pages/{id}                                 satu halaman beserta drafnya
//	PUT    /pages/{id}                                 simpan pengaturan dan draf
//	DELETE /pages/{id}                                 hapus; menjawab 204
//	POST   /pages/{id}/publish                         simpan lalu terbitkan
//	POST   /pages/{id}/unpublish                       tarik dari pengunjung
//	GET    /pages/{id}/revisions                       riwayat terbitan
//	POST   /pages/{id}/revisions/{revision}/restore    terbitan itu menjadi draf
//	POST   /pages/{id}/images                          body: isi gambar blok; menjawab 201 dan berkasnya
//	PUT    /pages/{id}/images/seo                      body: isi gambar pratinjau
//	DELETE /pages/{id}/images/seo                      hapus gambar pratinjau
func (s *Service) Routes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/pages", Handler: s.handleList},
		{Method: http.MethodPost, Path: "/pages", Handler: s.handleCreate},
		{Method: http.MethodPost, Path: "/pages/import-legacy", Handler: s.handleImport},
		{Method: http.MethodGet, Path: "/pages/{id}", Handler: s.handleGet},
		{Method: http.MethodPut, Path: "/pages/{id}", Handler: s.handleUpdate},
		{Method: http.MethodDelete, Path: "/pages/{id}", Handler: s.handleDelete},
		{Method: http.MethodPost, Path: "/pages/{id}/publish", Handler: s.handlePublish},
		{Method: http.MethodPost, Path: "/pages/{id}/unpublish", Handler: s.handleUnpublish},
		{Method: http.MethodGet, Path: "/pages/{id}/revisions", Handler: s.handleRevisions},
		{Method: http.MethodPost, Path: "/pages/{id}/revisions/{revision}/restore", Handler: s.handleRestore},
		{Method: http.MethodPost, Path: "/pages/{id}/images", Handler: s.handleUpload},
		{Method: http.MethodPut, Path: "/pages/{id}/images/seo", Handler: s.handleSetSEOImage},
		{Method: http.MethodDelete, Path: "/pages/{id}/images/seo", Handler: s.handleRemoveSEOImage},
	}
}

// PublicRoutes adalah endpoint TANPA SESI, dipasang di akar situs:
//
//	GET /page.json?path=/layanan    halaman terbit (Public); 404 bila tidak ada
//
// Untuk frontend yang berpindah halaman tanpa memuat ulang. Halaman yang
// dibuka langsung sudah membawa datanya lewat RenderPage.
func (s *Service) PublicRoutes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: PublicPath, Handler: s.handlePublic},
	}
}

func (s *Service) respond(w http.ResponseWriter, r *http.Request, status int, out any, err error) {
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, status, out)
}

// authorized memeriksa izin SEBELUM body dibaca: yang tidak berhak tidak
// perlu tahu isiannya salah di mana.
func (s *Service) authorized(w http.ResponseWriter, r *http.Request) bool {
	if err := s.hooks.Authorize(r.Context(), Manage); err != nil {
		s.hooks.WriteError(w, r, err)
		return false
	}
	return true
}

func (s *Service) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !s.authorized(w, r) {
		return false
	}
	if err := httpjson.Decode(w, r, dst); err != nil {
		s.hooks.WriteError(w, r, err)
		return false
	}
	return true
}

// segment membaca id sesudah segmen name di path; router tidak dilibatkan.
func segment(r *http.Request, name string) (uuid.UUID, bool) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == name {
			id, err := uuid.Parse(parts[i+1])
			return id, err == nil && id.String() == parts[i+1]
		}
	}
	return uuid.Nil, false
}

// pageID membaca {id}; id yang salah bentuk dijawab "tidak ditemukan".
func (s *Service) pageID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := segment(r, "pages")
	if !ok {
		s.hooks.WriteError(w, r, errNotFound)
	}
	return id, ok
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	out, err := s.List(r.Context())
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in CreateInput
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.Create(r.Context(), in)
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Service) handleImport(w http.ResponseWriter, r *http.Request) {
	out, err := s.ImportLegacy(r.Context())
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	out, err := s.Get(r.Context(), id)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !s.decode(w, r, &in) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	out, err := s.Update(r.Context(), id, in)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handlePublish(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !s.decode(w, r, &in) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	out, err := s.Publish(r.Context(), id, in)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	out, err := s.Unpublish(r.Context(), id)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	if err := s.Delete(r.Context(), id); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleRevisions(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	out, err := s.Revisions(r.Context(), id)
	s.respond(w, r, http.StatusOK, map[string]any{"data": out}, err)
}

func (s *Service) handleRestore(w http.ResponseWriter, r *http.Request) {
	var in RestoreInput
	if !s.decode(w, r, &in) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	revision, ok := segment(r, "revisions")
	if !ok {
		s.hooks.WriteError(w, r, appkit.NotFound("Terbitan tidak ditemukan."))
		return
	}
	out, err := s.Restore(r.Context(), id, revision, in)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	// Satu byte di atas batas, supaya media yang menyebut batasnya.
	body := http.MaxBytesReader(w, r.Body, s.files.MaxBytes()+1)
	out, err := s.UploadImage(r.Context(), id, body)
	s.respond(w, r, http.StatusCreated, out, err)
}

func (s *Service) handleSetSEOImage(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.files.MaxBytes()+1)
	out, err := s.SetSEOImage(r.Context(), id, body)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handleRemoveSEOImage(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id, ok := s.pageID(w, r)
	if !ok {
		return
	}
	out, err := s.RemoveSEOImage(r.Context(), id)
	s.respond(w, r, http.StatusOK, out, err)
}

func (s *Service) handlePublic(w http.ResponseWriter, r *http.Request) {
	org, err := s.sites.PublicOrganization(r)
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	page, _, ok, err := s.Lookup(r.Context(), org, r.URL.Query().Get("path"))
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	if !ok {
		s.hooks.WriteError(w, r, errNotFound)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(15))
	httpjson.Write(w, http.StatusOK, page)
}
