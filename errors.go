package appkit

// Kind adalah jenis galat yang boleh ditampilkan ke pengguna. Produk
// memetakannya ke kode dan status HTTP miliknya di Hooks.WriteError.
type Kind string

const (
	// KindValidation: isian tidak sah. Fields menyebut field-nya.
	KindValidation Kind = "validation"
	// KindNotFound: yang diminta tidak ada. Dipakai juga untuk data milik
	// organization lain — keberadaannya tidak boleh bocor.
	KindNotFound Kind = "not_found"
	// KindConflict: data sudah diubah orang lain sejak dibaca. Pengguna perlu
	// memuat ulang, bukan mengulang kiriman yang sama.
	KindConflict Kind = "conflict"
	// KindQuotaExceeded: batas pemakaian organization itu sudah penuh,
	// misalnya kuota penyimpanan paketnya. Produk memetakannya ke tawaran naik
	// paket, bukan ke galat isian.
	KindQuotaExceeded Kind = "quota_exceeded"
)

// FieldError menunjuk satu field yang tidak sah. Field memakai nama yang sama
// dengan body JSON, sehingga formulir dapat menandainya.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error adalah galat modul yang pesannya aman ditampilkan ke pengguna.
type Error struct {
	Kind    Kind
	Message string
	Fields  []FieldError
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Message }

// Validation mengembalikan galat isian beserta rincian per field.
func Validation(message string, fields ...FieldError) *Error {
	return &Error{Kind: KindValidation, Message: message, Fields: fields}
}

// NotFound mengembalikan galat "tidak ditemukan".
func NotFound(message string) *Error {
	return &Error{Kind: KindNotFound, Message: message}
}

// Conflict mengembalikan galat "sudah diubah orang lain".
func Conflict(message string) *Error {
	return &Error{Kind: KindConflict, Message: message}
}

// QuotaExceeded mengembalikan galat "batas pemakaian penuh".
func QuotaExceeded(message string) *Error {
	return &Error{Kind: KindQuotaExceeded, Message: message}
}
