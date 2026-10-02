package audit

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
	"github.com/gonsutrijayautama/gonsu-appkit-go/internal/httpjson"
)

// Routes adalah endpoint jejak audit, relatif terhadap akar API produk
// (`/v1`). Pasang di balik middleware sesi:
//
//	GET /audit-events?category=&before=&limit=   catatan, terbaru dulu (izin View)
//
// Tidak ada endpoint untuk menulis: catatan dibuat kode server, tidak pernah
// dari permintaan klien.
func (s *Service) Routes() []appkit.Route {
	return []appkit.Route{
		{Method: http.MethodGet, Path: "/audit-events", Handler: s.handleList},
	}
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	// Izin diperiksa sebelum saringan dibaca: yang tidak berhak tidak perlu
	// tahu saringannya salah di mana. List memeriksanya lagi.
	if err := s.hooks.Authorize(r.Context(), View); err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	query := r.URL.Query()
	q := Query{Category: Category(query.Get("category"))}
	// Nilai limit yang tidak valid jatuh ke bawaan: ini parameter tampilan.
	q.Limit, _ = strconv.Atoi(query.Get("limit"))
	if before := query.Get("before"); before != "" {
		id, err := uuid.Parse(before)
		if err != nil {
			s.hooks.WriteError(w, r, appkit.Validation("Saringan belum sesuai.",
				appkit.FieldError{Field: "before", Message: "Penanda halaman tidak sah."}))
			return
		}
		q.Before = id
	}
	page, err := s.List(r.Context(), q)
	if err != nil {
		s.hooks.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, page)
}
