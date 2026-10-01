// Package httpjson membaca dan menulis JSON untuk handler modul.
package httpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	appkit "github.com/gonsutrijayautama/gonsu-appkit-go"
)

// MaxBodyBytes membatasi body JSON.
const MaxBodyBytes = 1 << 20

// Decode mengurai body JSON secara ketat ke dst: field yang tidak dikenal
// ditolak, supaya salah ketik nama field di klien tidak lolos diam-diam
// sebagai nilai kosong.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return appkit.Validation("Body permintaan terlalu besar.")
		}
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && typeErr.Field != "" {
			return appkit.Validation("Isian belum sesuai.", appkit.FieldError{
				Field: typeErr.Field, Message: "Jenis isinya tidak sesuai.",
			})
		}
		// encoding/json tidak punya tipe galat untuk field tak dikenal; bentuk
		// pesannya `json: unknown field "x"` stabil sejak Go 1.10.
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return appkit.Validation("Isian belum sesuai.", appkit.FieldError{
				Field: strings.Trim(name, `"`), Message: "Field ini tidak dikenal.",
			})
		}
		return appkit.Validation("Body permintaan bukan JSON yang valid.")
	}
	if dec.More() {
		return appkit.Validation("Body permintaan harus berisi tepat satu objek JSON.")
	}
	return nil
}

// Write menulis v sebagai JSON dengan status yang diberikan.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
