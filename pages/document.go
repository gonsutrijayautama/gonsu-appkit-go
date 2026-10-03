package pages

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/microcosm-cc/bluemonday"

	"github.com/gonsutrijayautama/gonsu-appkit-go/media"
)

// FieldKind adalah jenis sebuah isian blok, yang menentukan cara server
// memeriksanya.
type FieldKind string

const (
	// FieldText: teks biasa. Dirender produk apa adanya (di-escape), jadi
	// tidak dibersihkan sebagai HTML.
	FieldText FieldKind = "text"
	// FieldRichText: HTML berformat terbatas. Dibersihkan saat disimpan:
	// hanya p, h2–h4, strong, em, u, s, a, ul, ol, li, blockquote, hr, dan br,
	// dan tautan hanya https, alamat situs sendiri, atau jangkar.
	FieldRichText FieldKind = "richtext"
	// FieldLink: tautan, mis. tujuan tombol. Kosong, https, "/alamat", atau
	// "#jangkar".
	FieldLink FieldKind = "link"
	// FieldImage: gambar. Kosong, atau alamat berkas media milik organization
	// itu sendiri ("/media/<id>"). Data URL dan alamat luar ditolak.
	FieldImage FieldKind = "image"
	// FieldValue: nilai lain — angka, pilihan, ya/tidak.
	FieldValue FieldKind = "value"
	// FieldSlot: daftar blok di dalam blok, mis. isi sebuah kolom.
	FieldSlot FieldKind = "slot"
	// FieldList: daftar butir yang masing-masing berisian Field.Fields, mis.
	// kartu layanan.
	FieldList FieldKind = "list"
	// FieldObject: satu objek berisian Field.Fields.
	FieldObject FieldKind = "object"
)

func (k FieldKind) valid() bool {
	switch k {
	case FieldText, FieldRichText, FieldLink, FieldImage, FieldValue, FieldSlot, FieldList, FieldObject:
		return true
	}
	return false
}

// Field adalah satu isian blok.
type Field struct {
	Kind FieldKind
	// Fields adalah isian tiap butir FieldList, atau isian FieldObject.
	Fields map[string]Field
}

// Block adalah satu jenis blok yang boleh dipakai di halaman. Type dan
// isiannya harus sama dengan konfigurasi penyusun halaman di frontend produk.
type Block struct {
	Type   string
	Fields map[string]Field
}

// Batas isi halaman. Isi halaman adalah susunan blok, bukan tempat berkas:
// gambar disimpan lewat media.
const (
	DefaultMaxDocumentBytes = 256 << 10
	maxBlocks               = 300
	maxDepth                = 5
	maxListItems            = 50
	maxText                 = 5000
	maxRichText             = 20000
	maxLink                 = 300
	maxValue                = 500
	maxBlockID              = 100
)

var (
	reBlockType = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,59}$`)
	reFieldName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,59}$`)
	reBlockID   = regexp.MustCompile(`^[A-Za-z0-9_:-]+$`)
)

// richText adalah aturan pembersihan FieldRichText.
func richText() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "h2", "h3", "h4", "strong", "em", "u", "s", "ul", "ol", "li", "blockquote", "hr", "br")
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("https")
	p.AllowRelativeURLs(true)
	p.RequireParseableURLs(true)
	return p
}

// checker memeriksa dan merapikan satu dokumen halaman.
type checker struct {
	blocks map[string]Block
	root   map[string]Field
	policy *bluemonday.Policy
	// media mengumpulkan berkas media yang dirujuk dokumen.
	media  map[uuid.UUID]bool
	count  int
	errors []string
}

func (c *checker) fail(path, message string) {
	if len(c.errors) < 10 {
		c.errors = append(c.errors, path+": "+message)
	}
}

// checkDocument memeriksa data penyusun halaman raw dan mengembalikan versi
// yang sudah dirapikan — teks berformat dibersihkan — beserta berkas media
// yang dirujuknya. problems kosong berarti sah.
func (s *Service) checkDocument(raw json.RawMessage) (clean json.RawMessage, refs map[uuid.UUID]bool, problems []string) {
	if len(raw) == 0 {
		return nil, nil, []string{"isi halaman kosong"}
	}
	if len(raw) > s.opts.MaxDocumentBytes {
		return nil, nil, []string{fmt.Sprintf("isi halaman melebihi %d KB", s.opts.MaxDocumentBytes>>10)}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil, nil, []string{"isi halaman bukan objek JSON"}
	}
	c := &checker{blocks: s.blocks, root: s.opts.Root, policy: s.policy, media: map[uuid.UUID]bool{}}
	for key := range doc {
		switch key {
		case "root", "content", "zones":
		default:
			c.fail(key, "bagian tidak dikenal")
		}
	}

	root, _ := doc["root"].(map[string]any)
	if doc["root"] != nil && root == nil {
		c.fail("root", "harus objek")
	}
	if root != nil {
		for key := range root {
			if key != "props" {
				c.fail("root."+key, "bagian tidak dikenal")
			}
		}
		if props, ok := root["props"]; ok {
			obj, ok := props.(map[string]any)
			if !ok {
				c.fail("root.props", "harus objek")
			} else {
				root["props"] = c.fields("root.props", obj, c.root, 0, false)
			}
		}
	}

	content, ok := doc["content"].([]any)
	if !ok {
		c.fail("content", "harus daftar blok")
	} else {
		doc["content"] = c.list("content", content, 0)
	}

	// zones: tempat blok bersarang cara lama penyusun halaman ("<id blok>:<nama>").
	if z, present := doc["zones"]; present {
		zones, ok := z.(map[string]any)
		if !ok {
			c.fail("zones", "harus objek")
		}
		for name, v := range zones {
			items, ok := v.([]any)
			if !ok || len(name) > 200 {
				c.fail("zones."+name, "harus daftar blok")
				continue
			}
			zones[name] = c.list("zones."+name, items, 1)
		}
	}

	if len(c.errors) > 0 {
		return nil, nil, c.errors
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, []string{"isi halaman tidak dapat disimpan"}
	}
	return out, c.media, nil
}

// list memeriksa daftar blok pada kedalaman depth.
func (c *checker) list(path string, items []any, depth int) []any {
	if depth > maxDepth {
		c.fail(path, fmt.Sprintf("blok bersarang lebih dari %d tingkat", maxDepth))
		return items
	}
	for i, item := range items {
		c.count++
		if c.count > maxBlocks {
			c.fail(path, fmt.Sprintf("lebih dari %d blok", maxBlocks))
			return items
		}
		at := fmt.Sprintf("%s[%d]", path, i)
		block, ok := item.(map[string]any)
		if !ok {
			c.fail(at, "harus objek blok")
			continue
		}
		kind, _ := block["type"].(string)
		def, known := c.blocks[kind]
		if !known {
			c.fail(at, fmt.Sprintf("jenis blok %q tidak dikenal", kind))
			continue
		}
		for key := range block {
			if key != "type" && key != "props" {
				c.fail(at+"."+key, "bagian tidak dikenal")
			}
		}
		props, ok := block["props"].(map[string]any)
		if !ok {
			c.fail(at+".props", "harus objek")
			continue
		}
		id, _ := props["id"].(string)
		if id == "" || len(id) > maxBlockID || !reBlockID.MatchString(id) {
			c.fail(at+".props.id", "id blok tidak sah")
		}
		block["props"] = c.fields(at+".props", props, def.Fields, depth, true)
	}
	return items
}

// fields memeriksa isian sebuah objek menurut defs. withID: objek itu props
// blok, yang membawa id-nya sendiri.
func (c *checker) fields(path string, obj map[string]any, defs map[string]Field, depth int, withID bool) map[string]any {
	for key, v := range obj {
		if withID && key == "id" {
			continue
		}
		def, ok := defs[key]
		if !ok {
			c.fail(path+"."+key, "isian tidak dikenal")
			continue
		}
		obj[key] = c.value(path+"."+key, v, def, depth)
	}
	return obj
}

func (c *checker) value(path string, v any, def Field, depth int) any {
	if v == nil {
		return nil
	}
	switch def.Kind {
	case FieldText:
		s, ok := v.(string)
		if !ok || utf8.RuneCountInString(s) > maxText {
			c.fail(path, fmt.Sprintf("harus teks, maksimal %d karakter", maxText))
		}
		return v
	case FieldRichText:
		s, ok := v.(string)
		if !ok || utf8.RuneCountInString(s) > maxRichText {
			c.fail(path, fmt.Sprintf("harus teks berformat, maksimal %d karakter", maxRichText))
			return v
		}
		return c.policy.Sanitize(s)
	case FieldLink:
		s, ok := v.(string)
		if !ok || !validLink(s) {
			c.fail(path, "tautan harus alamat https, alamat situs ini (\"/…\"), atau jangkar (\"#…\")")
		}
		return v
	case FieldImage:
		s, ok := v.(string)
		if !ok {
			c.fail(path, "gambar harus alamat berkas")
			return v
		}
		if s == "" {
			return v
		}
		id, err := uuid.Parse(strings.TrimPrefix(s, media.PublicPath))
		if !strings.HasPrefix(s, media.PublicPath) || err != nil || media.URL(id) != s {
			c.fail(path, "gambar harus diunggah lewat halaman ini")
			return v
		}
		c.media[id] = true
		return v
	case FieldValue:
		switch x := v.(type) {
		case bool, json.Number:
		case string:
			if utf8.RuneCountInString(x) > maxValue {
				c.fail(path, fmt.Sprintf("nilai maksimal %d karakter", maxValue))
			}
		default:
			c.fail(path, "harus angka, teks pendek, atau ya/tidak")
		}
		return v
	case FieldSlot:
		items, ok := v.([]any)
		if !ok {
			c.fail(path, "harus daftar blok")
			return v
		}
		return c.list(path, items, depth+1)
	case FieldList:
		items, ok := v.([]any)
		if !ok || len(items) > maxListItems {
			c.fail(path, fmt.Sprintf("harus daftar, maksimal %d butir", maxListItems))
			return v
		}
		for i, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				c.fail(fmt.Sprintf("%s[%d]", path, i), "harus objek")
				continue
			}
			items[i] = c.fields(fmt.Sprintf("%s[%d]", path, i), obj, def.Fields, depth, false)
		}
		return items
	case FieldObject:
		obj, ok := v.(map[string]any)
		if !ok {
			c.fail(path, "harus objek")
			return v
		}
		return c.fields(path, obj, def.Fields, depth, false)
	}
	c.fail(path, "jenis isian tidak dikenal")
	return v
}

// validLink: kosong, https lengkap, alamat situs ini ("/…", bukan "//…"), atau
// jangkar ("#…"). Skema lain — javascript:, data:, http: — tidak pernah
// sampai ke halaman publik.
func validLink(s string) bool {
	if s == "" {
		return true
	}
	if utf8.RuneCountInString(s) > maxLink || strings.ContainsAny(s, " \t\r\n\\") {
		return false
	}
	switch {
	case strings.HasPrefix(s, "#"):
		return true
	case strings.HasPrefix(s, "/"):
		return !strings.HasPrefix(s, "//")
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// checkCatalog memeriksa katalog blok saat New.
func checkCatalog(blocks []Block, root map[string]Field) (map[string]Block, error) {
	if len(blocks) == 0 {
		return nil, fmt.Errorf("pages: Options.Blocks wajib diisi")
	}
	out := make(map[string]Block, len(blocks))
	for _, b := range blocks {
		if !reBlockType.MatchString(b.Type) {
			return nil, fmt.Errorf("pages: jenis blok %q tidak sah", b.Type)
		}
		if _, dup := out[b.Type]; dup {
			return nil, fmt.Errorf("pages: jenis blok %s terdaftar dua kali", b.Type)
		}
		if err := checkFields(b.Type, b.Fields); err != nil {
			return nil, err
		}
		out[b.Type] = b
	}
	if err := checkFields("root", root); err != nil {
		return nil, err
	}
	return out, nil
}

func checkFields(owner string, fields map[string]Field) error {
	for name, f := range fields {
		switch {
		case !reFieldName.MatchString(name) || name == "id":
			return fmt.Errorf("pages: nama isian %s.%s tidak sah", owner, name)
		case !f.Kind.valid():
			return fmt.Errorf("pages: jenis isian %s.%s tidak dikenal: %q", owner, name, f.Kind)
		case (f.Kind == FieldList || f.Kind == FieldObject) && len(f.Fields) == 0:
			return fmt.Errorf("pages: isian %s.%s wajib menyebut Fields", owner, name)
		case f.Kind != FieldList && f.Kind != FieldObject && len(f.Fields) > 0:
			return fmt.Errorf("pages: isian %s.%s tidak boleh punya Fields", owner, name)
		}
		if err := checkFields(owner+"."+name, f.Fields); err != nil {
			return err
		}
	}
	return nil
}
