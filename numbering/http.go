package numbering

import (
	"net/http"
	"path"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// Routes adalah endpoint pengaturan penomoran dokumen, relatif terhadap akar
// API produk (`/v1`). Pasang di balik middleware sesi; keduanya meminta izin
// Manage:
//
//	GET /document-numbering           skema setiap jenis dokumen dan daftar token (Listing)
//	PUT /document-numbering/{type}    simpan pola dan kebijakan reset; menjawab skemanya
//
// Riwayat perubahannya dibaca dari jejak audit (package audit). Next tidak
// punya endpoint: nomor dialokasikan modul dokumen milik produk, di transaksi
// dokumennya.
func (s *Service) Routes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/document-numbering", Handler: s.handleList},
		{Method: http.MethodPut, Path: "/document-numbering/{type}", Handler: s.handleUpdate},
	}
}

func (s *Service) respond(w http.ResponseWriter, r *http.Request, out any, err error) {
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

// typeOf membaca Type.Key dari segmen terakhir path; router tidak dilibatkan.
func typeOf(r *http.Request) string { return path.Base(r.URL.Path) }

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	schemes, err := s.Schemes(r.Context())
	s.respond(w, r, Listing{Data: schemes, Tokens: Tokens()}, err)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	// Izin diperiksa sebelum body dibaca: yang tidak berhak tidak perlu tahu
	// isiannya salah di mana. SetScheme memeriksanya lagi.
	if err := s.hooks.Authorize(r.Context(), Manage); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	var in Input
	if err := httpjson.Decode(w, r, &in); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	out, err := s.SetScheme(r.Context(), typeOf(r), in)
	s.respond(w, r, out, err)
}
