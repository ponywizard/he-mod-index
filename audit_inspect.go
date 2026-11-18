package main

// The inspector reads bytes, never Godot resources. In particular it must not
// use a Godot loader: loading resources can itself execute embedded scripts.
import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const auditPolicy = "static-pilot-v1"
const auditMaxPackage = 64 << 20
const auditMaxExpanded = 192 << 20
const auditMaxFiles = 2000
const auditMaxText = 120000

type auditFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
	Kind   string `json:"kind"` // text, asset, numeric_data, container, opaque
	Text   string `json:"text,omitempty"`
	SameAs string `json:"same_as,omitempty"`
}
type auditIssue struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}
type auditInspection struct {
	Policy string       `json:"policy"`
	SHA256 string       `json:"sha256"`
	Size   int          `json:"size"`
	Files  []auditFile  `json:"files"`
	Issues []auditIssue `json:"issues"`
}
type auditInspector struct {
	auditInspection
	expanded, textBytes int
	textHashes          map[string]string
}

func auditHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s *auditInspector) issue(p, code, detail string) {
	if len(s.Issues) < auditMaxFiles {
		s.Issues = append(s.Issues, auditIssue{p, code, detail})
	}
}

func inspectAuditBytes(b []byte) auditInspection {
	s := auditInspector{auditInspection: auditInspection{Policy: auditPolicy, SHA256: auditHash(b), Size: len(b), Files: []auditFile{}, Issues: []auditIssue{}}, textHashes: map[string]string{}}
	if len(b) > auditMaxPackage {
		s.issue("package", "limit", "Package exceeds 64 MiB")
	} else {
		s.visit("package", b, 0)
	}
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
	return s.auditInspection
}

func auditMemberPath(raw string, godot bool) (string, error) {
	if godot {
		raw = strings.TrimPrefix(raw, "res://")
	}
	if raw == "" || len(raw) > 1024 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\:\x00\r\n") || strings.HasPrefix(raw, "/") {
		return "", errors.New("invalid member path")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." || part == "." {
			return "", errors.New("relative traversal path")
		}
	}
	// Escape our nested-container separator instead of rejecting legitimate
	// filenames such as Level Up!.mp3. Escape % first to prevent collisions.
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "%", "%25"), "!", "%21")
	return strings.TrimSuffix(raw, "/"), nil
}

func (s *auditInspector) visit(p string, b []byte, depth int) {
	if len(s.Files) >= auditMaxFiles || s.expanded+len(b) > auditMaxExpanded {
		s.issue(p, "limit", "File count or expanded byte limit reached")
		return
	}
	s.expanded += len(b)
	f := auditFile{Path: p, SHA256: auditHash(b), Size: len(b), Kind: "opaque"}
	isPCK := bytes.HasPrefix(b, []byte("GDPC"))
	isZip := bytes.HasPrefix(b, []byte("PK\x03\x04")) || bytes.HasPrefix(b, []byte("PK\x05\x06"))
	if isPCK || isZip {
		f.Kind = "container"
		s.Files = append(s.Files, f)
		if depth >= 3 {
			s.issue(p, "limit", "Nested package depth exceeds three")
			return
		}
		if isPCK {
			s.pck(p, b, depth+1)
		} else {
			s.zip(p, b, depth+1)
		}
		return
	}
	ext := strings.ToLower(path.Ext(p))
	if ext == ".gde" || bytes.HasPrefix(b, []byte("GDEC")) {
		s.issue(p, "encrypted", "Encrypted script; review blocked")
	} else if bytes.HasPrefix(b, []byte("GDSC")) || ext == ".gdc" {
		s.issue(p, "unsupported", "Compiled GDScript requires a supported decompiler")
	} else if auditAsset(b, ext) {
		f.Kind = "asset"
	} else if len(b) == 0 || utf8.Valid(b) && !bytes.ContainsRune(b, 0) {
		// Arrays containing only JSON numbers, booleans, null and arrays cannot
		// carry script text. Hash large mesh coordinate tables without sending
		// thousands of numbers to the model. Objects/strings are never omitted.
		if ext == ".json" && len(b) > 4096 && auditNumericJSON(b) {
			f.Kind = "numeric_data"
		} else if prior := s.textHashes[f.SHA256]; prior != "" {
			f.Kind, f.SameAs = "text_alias", prior
		} else if len(b) > auditMaxText || s.textBytes+len(b) > auditMaxText {
			s.issue(p, "limit", "Text exceeds review context limit; not silently truncated")
		} else {
			f.Kind, f.Text = "text", string(b)
			s.textBytes += len(b)
			s.textHashes[f.SHA256] = p
		}
	} else {
		s.issue(p, "unsupported", "Opaque binary, executable, or resource; no complete source review")
	}
	s.Files = append(s.Files, f)
}

func auditNumericJSON(b []byte) bool {
	if bytes.ContainsAny(b, "\"{}") || !json.Valid(b) {
		return false
	}
	return true
}

// Media are inventoried and hashed, not loaded/decoded. This is a source-code
// review, not a claim to detect crafted media exploits. Both extension and magic
// must agree; arbitrary bytes renamed .png do not disappear from coverage.
func auditAsset(b []byte, ext string) bool {
	switch ext {
	case ".png":
		return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n"))
	case ".jpg", ".jpeg":
		return bytes.HasPrefix(b, []byte{255, 216, 255})
	case ".ogg":
		return bytes.HasPrefix(b, []byte("OggS"))
	case ".mp3":
		return bytes.HasPrefix(b, []byte("ID3")) || len(b) > 3 && b[0] == 255 && b[1]&0xe0 == 0xe0
	case ".wav":
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE"
	case ".webp":
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	}
	return false
}

func (s *auditInspector) zip(p string, b []byte, depth int) {
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		s.issue(p, "malformed", "Unreadable ZIP directory")
		return
	}
	if len(z.File) > auditMaxFiles {
		s.issue(p, "limit", "ZIP has too many entries")
		return
	}
	seen := map[string]bool{}
	for _, entry := range z.File {
		name, err := auditMemberPath(entry.Name, false)
		if err != nil {
			s.issue(p, "unsafe_path", "ZIP contains an unsafe member path")
			continue
		}
		member := p + "!/" + name
		if entry.Flags&0x41 != 0 || entry.Method == 99 {
			s.issue(member, "encrypted", "Encrypted ZIP member; review blocked")
			continue
		}
		if seen[name] {
			s.issue(member, "duplicate_path", "Duplicate ZIP member")
			continue
		}
		seen[name] = true
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.Mode().IsRegular() {
			s.issue(member, "unsafe_path", "ZIP symlink or non-regular file")
			continue
		}
		if entry.UncompressedSize64 > uint64(auditMaxExpanded-s.expanded) || entry.UncompressedSize64 > 1<<20 && (entry.CompressedSize64 == 0 || entry.UncompressedSize64/entry.CompressedSize64 > 200) {
			s.issue(member, "limit", "ZIP expansion or compression ratio exceeds limit")
			continue
		}
		r, err := entry.Open()
		if err != nil {
			s.issue(member, "unsupported", "Unsupported ZIP compression")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(r, int64(auditMaxExpanded-s.expanded)+1))
		r.Close()
		if err != nil || len(data) != int(entry.UncompressedSize64) {
			s.issue(member, "malformed", "ZIP size or checksum mismatch")
			continue
		}
		s.visit(member, data, depth)
	}
}

// Format documented by Godot core/io/file_access_pack.{cpp,h}. No resource
// decoding, embedded executable search, sparse bundles, delta or key guessing.
func (s *auditInspector) pck(p string, b []byte, depth int) {
	bad := func() { s.issue(p, "malformed", "Invalid PCK directory or member bounds") }
	if len(b) < 40 {
		bad()
		return
	}
	u32 := func(offset int) uint32 { return binary.LittleEndian.Uint32(b[offset:]) }
	u64 := func(offset int) uint64 { return binary.LittleEndian.Uint64(b[offset:]) }
	version, flags, base := u32(4), u32(20), u64(24)
	if flags&1 != 0 {
		s.issue(p, "encrypted", "Encrypted PCK directory; review blocked")
		return
	}
	if (version != 2 && version != 3 && version != 4) || flags & ^uint32(3) != 0 {
		s.issue(p, "unsupported", "Unsupported PCK version, sparse bundle or flags")
		return
	}
	directory := uint64(96)
	if version >= 3 {
		directory = u64(32)
	}
	if directory > uint64(len(b)-4) || base > uint64(len(b)) {
		bad()
		return
	}
	off := int(directory)
	count := u32(off)
	off += 4
	if count > auditMaxFiles {
		s.issue(p, "limit", "PCK has too many entries")
		return
	}
	type member struct {
		name        string
		start, size uint64
		checksum    []byte
		flags       uint32
	}
	members := make([]member, 0, int(count))
	seen := map[string]bool{}
	for i := uint32(0); i < count; i++ {
		if off > len(b)-4 {
			bad()
			return
		}
		n := u32(off)
		off += 4
		if n == 0 || n > 1024 || int(n) > len(b)-off-36 {
			bad()
			return
		}
		raw := bytes.TrimRight(b[off:off+int(n)], "\x00")
		name, err := auditMemberPath(string(raw), true)
		off += int(n)
		start, size := u64(off), u64(off+8)
		checksum, fileFlags := b[off+16:off+32], u32(off+32)
		off += 36
		if fileFlags&1 != 0 {
			s.issue(p, "encrypted", "Encrypted PCK member; review blocked")
			continue
		}
		if err != nil || seen[name] {
			s.issue(p, "unsafe_path", fmt.Sprintf("Unsafe or duplicate PCK member path: %q", raw))
			continue
		}
		seen[name] = true
		if fileFlags != 0 {
			s.issue(p+"!/"+name, "unsupported", "PCK removal, delta or unknown member flags")
			continue
		}
		if start > uint64(len(b))-base || size > uint64(len(b))-base-start {
			bad()
			return
		}
		members = append(members, member{name, start + base, size, checksum, fileFlags})
	}
	for _, m := range members {
		if m.start < 40 || m.start < uint64(off) && m.start+m.size > directory {
			bad()
			continue
		}
		data := b[m.start : m.start+m.size]
		// MD5 is a format integrity check only; identity always uses SHA-256.
		sum := md5.Sum(data)
		if !bytes.Equal(sum[:], m.checksum) {
			s.issue(p+"!/"+m.name, "malformed", "PCK member checksum mismatch")
			continue
		}
		s.visit(p+"!/"+m.name, data, depth)
	}
}

func validateAuditInspection(in auditInspection) error {
	if in.Policy != auditPolicy || len(in.SHA256) != 64 || in.Size < 0 || in.Size > auditMaxPackage || len(in.Files) > auditMaxFiles || len(in.Issues) > auditMaxFiles {
		return errors.New("invalid inspection header")
	}
	if _, err := hex.DecodeString(in.SHA256); err != nil {
		return err
	}
	seen, total := map[string]bool{}, 0
	for _, f := range in.Files {
		if seen[f.Path] || f.Path == "" || len(f.Path) > 4096 || len(f.SHA256) != 64 || f.Size < 0 || f.Size > auditMaxExpanded {
			return errors.New("invalid inspection inventory")
		}
		seen[f.Path] = true
		if _, err := hex.DecodeString(f.SHA256); err != nil {
			return err
		}
		switch f.Kind {
		case "text":
			if len(f.Text) != f.Size || auditHash([]byte(f.Text)) != f.SHA256 {
				return fmt.Errorf("text hash mismatch: %s", f.Path)
			}
			total += len(f.Text)
		case "container", "asset", "numeric_data", "opaque", "text_alias":
			if f.Text != "" {
				return errors.New("unexpected non-text payload")
			}
		default:
			return errors.New("unknown inventory kind")
		}
	}
	files := map[string]auditFile{}
	for _, f := range in.Files {
		files[f.Path] = f
	}
	for _, f := range in.Files {
		if f.Kind == "text_alias" {
			prior := files[f.SameAs]
			if prior.Kind != "text" || prior.SHA256 != f.SHA256 || prior.Size != f.Size {
				return errors.New("invalid text alias")
			}
		} else if f.SameAs != "" {
			return errors.New("unexpected text alias")
		}
	}
	if total > auditMaxText {
		return errors.New("inspection text exceeds limit")
	}
	return nil
}
