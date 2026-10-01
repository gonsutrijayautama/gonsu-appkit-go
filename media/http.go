package media

import (
	"io"
	"net/http"
	"path"
	"strconv"

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// PublicRoutes adalah endpoint TANPA SESI, dipasang di akar situs:
//
//	GET /media/{id}
//
// Pasang di luar middleware sesi dan hak pakai: logo tampil di halaman depan
// dan halaman masuk, sebelum ada yang login.
func (s *Service) PublicRoutes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: PublicPath + "{id}", Handler: s.serve},
	}
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	// Id yang bukan UUID dijawab sama dengan berkas yang tidak ada.
	id, err := uuid.Parse(path.Base(r.URL.Path))
	if err != nil {
		s.writeError(w, r, errNotFound)
		return
	}
	f, rc, err := s.Open(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer rc.Close()

	h := w.Header()
	etag := `"` + f.sha256 + `"`
	h.Set("ETag", etag)
	// Isi di bawah satu id tidak pernah berubah.
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	// Berkas ini kiriman pengguna dan disajikan dari origin aplikasi. Jenisnya
	// sudah diperiksa saat disimpan; dua header ini memastikan peramban tidak
	// menafsirkannya sebagai hal lain bila dibuka langsung.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
