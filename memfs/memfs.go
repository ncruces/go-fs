// Package memfs implements an in memory fs.FS.
//
// The filesystem can be statically generated, or loaded (and modified) at runtime.
// It is safe for concurrent reads (not writes), and biased towards read performance.
//
// File names should be valid according to fs.ValidPath.
// Directories are implicit. Files can be gzip-compressed in memory.
// Methods are provided to serve gziped content directly to accepting HTTP clients.
//
// Usage:
//	assets, err = memfs.LoadCompressed(http.Dir("static"), gzip.BestCompression)
//	if err != nil {
//		log.Fatal(err)
//	}
//	log.Fatal(http.ListenAndServe("localhost:http", assets))
package memfs

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// FileSystem is the in memory fs.FS implementation.
type FileSystem struct {
	objs map[string]object
	dirs map[string][]string
}

// Create creates an empty FileSystem instance.
func Create() *FileSystem {
	return &FileSystem{
		objs: map[string]object{},
		dirs: map[string][]string{".": nil},
	}
}

// Load loads the contents of an fs.FS into a new FileSystem instance.
func Load(in fs.FS) (*FileSystem, error) {
	return LoadCompressed(in, gzip.NoCompression)
}

// LoadCompressed loads the contents of an fs.FS into a new FileSystem instance.
// Files are gzip-compressed with the specified compression level.
func LoadCompressed(in fs.FS, level int) (*FileSystem, error) {
	fsys := Create()
	err := fs.WalkDir(in, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if file, err := in.Open(path); err != nil {
			return err
		} else if info, err := d.Info(); err != nil {
			return err
		} else {
			return fsys.CreateCompressed(path, "", info.ModTime(), file, level)
		}
	})
	if err != nil {
		return nil, err
	}
	return fsys, nil
}

// Open implements fs.FS, opening files for reading.
// Compressed files are decompressed on-the-fly.
// Seeking compressed files is emulated and can be extremely slow.
func (fsys *FileSystem) Open(name string) (fs.File, error) {
	if o, ok := fsys.objs[name]; ok {
		if len(o.data) == o.size {
			return file{o, strings.NewReader(o.data)}, nil
		}
		return &zfile{object: o}, nil
	}
	if d, ok := fsys.dirs[name]; ok {
		return &dir{name: name, list: d, fsys: fsys}, nil
	}
	return nil, fs.ErrNotExist
}

// ReadFile implements fs.ReadFileFS, reading the named file and returning its contents.
// Compressed files are decompressed on-the-fly.
func (fsys *FileSystem) ReadFile(name string) ([]byte, error) {
	if o, ok := fsys.objs[name]; ok {
		if len(o.data) == o.size {
			return []byte(o.data), nil
		}

		gzip, err := gzip.NewReader(strings.NewReader(o.data))
		if err != nil {
			return nil, err
		}
		defer gzip.Close()
		return io.ReadAll(gzip)
	}
	if _, ok := fsys.dirs[name]; ok {
		return nil, fs.ErrInvalid
	}
	return nil, fs.ErrNotExist
}

// Stat implements fs.StatFS, returning a fs.FileInfo that describes the file.
func (fsys *FileSystem) Stat(name string) (fs.FileInfo, error) {
	return fsys.stat(name)
}

func (fsys *FileSystem) stat(name string) (entryInfo, error) {
	if o, ok := fsys.objs[name]; ok {
		return o, nil
	}
	if _, ok := fsys.dirs[name]; ok {
		return newDirInfo(name), nil
	}
	return nil, fs.ErrNotExist
}

// Create creates a file.
// Overwrites an existing file (but not a directory).
// Sniffs the MIME type if none is provided.
func (fsys *FileSystem) Create(name, mimetype string, modtime time.Time, r io.Reader) error {
	if !fs.ValidPath(name) {
		return fs.ErrInvalid
	}
	if _, ok := fsys.dirs[name]; ok {
		return fs.ErrExist
	}

	data, err := io.ReadAll(r)
	if err == nil {
		fsys.put(name, object{
			data: string(data),
			size: len(data),
			time: modtime,
			mime: getType(mimetype, name, data),
			hash: crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)),
		}, false)
	}
	return err
}

// CreateCompressed creates a compressed file.
// Overwrites an existing file (but not a directory).
// Files are gzip-compressed with the specified compression level.
// Sniffs the MIME type if none is provided.
func (fsys *FileSystem) CreateCompressed(name, mimetype string, modtime time.Time, r io.Reader, level int) error {
	if level == gzip.NoCompression {
		return fsys.Create(name, mimetype, modtime, r)
	}
	if !fs.ValidPath(name) {
		return fs.ErrInvalid
	}
	if _, ok := fsys.dirs[name]; ok {
		return fs.ErrExist
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if len(data) >= 1024 {
		var buf bytes.Buffer
		buf.Grow(len(data))

		gzip, err := gzip.NewWriterLevel(&buf, level)
		if err != nil {
			return err
		}
		defer gzip.Close()
		gzip.ModTime = modtime
		_, gzip.Name = path.Split(name)

		n, err := io.Copy(gzip, bytes.NewReader(data))
		if err == nil {
			err = gzip.Close()
		}
		if err == nil && 4*n >= 5*int64(buf.Len()) {
			fsys.put(name, object{
				data: buf.String(),
				size: len(data),
				time: modtime,
				mime: getType(mimetype, name, data),
				hash: getHash(buf.Bytes(), n),
			}, false)
			return nil
		}
	}

	return fsys.Create(name, mimetype, modtime, bytes.NewReader(data))
}

// CreateString creates a file from a string.
// This intended to be used by code generators.
// Bad things happen if you violate its expectations.
//
// Overwrites an existing file.
// Files are expected to be passed in fs.WalkDir order.
// MIME type will NOT be sniffed and content will NOT be compressed.
// If size != len(content), content is assumed to be gzip-compressed, and size its uncompressed size.
func (fsys *FileSystem) CreateString(name, mimetype string, modtime time.Time, hash uint32, size int, content string) {
	fsys.put(name, object{
		size: size,
		time: modtime,
		mime: mimetype,
		data: content,
		hash: hash,
	}, true)
}

func (fsys *FileSystem) put(name string, obj object, ordered bool) {
	dir, file := path.Split(name)
	obj.name = file
	fsys.objs[name] = obj

	hasFile := func(dir []string, name string) bool {
		if ordered {
			return len(dir) > 0 && dir[len(dir)-1] == name
		}
		for _, s := range dir {
			if s == name {
				return true
			}
		}
		return false
	}

	addFile := func(dir, name string) bool {
		d, ok := fsys.dirs[dir]
		if ok && hasFile(d, name) {
			return true
		}
		fsys.dirs[dir] = append(d, name)
		return false
	}

	for len(dir) > 0 {
		// remove trailing slash
		dir = dir[:len(dir)-1]
		if addFile(dir, name) {
			return
		}
		// continue with parent
		name = dir
		dir, _ = path.Split(dir)
	}
	addFile(".", name)
}

type object struct {
	name string
	data string
	size int
	time time.Time
	mime string
	hash uint32
}

func (o object) Name() string               { return o.name }
func (o object) IsDir() bool                { return false }
func (o object) Type() fs.FileMode          { return 0 }
func (o object) Info() (fs.FileInfo, error) { return o, nil }
func (o object) Size() int64                { return int64(o.size) }
func (o object) Mode() fs.FileMode          { return 0444 }
func (o object) ModTime() time.Time         { return o.time }
func (o object) Sys() interface{}           { return nil }

type file struct {
	object
	*strings.Reader
}

func (f file) Size() int64 {
	return int64(f.Len())
}

func (f file) Close() error {
	return nil
}

func (f file) Readdir(count int) ([]fs.FileInfo, error) {
	return nil, fs.ErrInvalid
}

func (f file) Stat() (fs.FileInfo, error) {
	return f, nil
}

type zfile struct {
	object
	pos    int
	reader *gzip.Reader
}

func (f *zfile) Close() error {
	f.pos = -1
	if c := f.reader; c != nil {
		f.reader = nil
		return c.Close()
	}
	return nil
}

func (f *zfile) Read(p []byte) (n int, err error) {
	if f.pos < 0 {
		return 0, fs.ErrClosed
	}
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if f.reader == nil {
		f.reader, err = gzip.NewReader(strings.NewReader(f.data))
		if err != nil {
			return 0, err
		}
		if f.pos > 0 {
			_, err = io.CopyN(io.Discard, f.reader, int64(f.pos))
			if err != nil {
				f.reader.Close()
				f.reader = nil
				return 0, err
			}
		}
	}
	n, err = f.reader.Read(p)
	f.pos += n
	return
}

func (f *zfile) Seek(offset int64, whence int) (int64, error) {
	if f.pos < 0 {
		return 0, fs.ErrClosed
	}
	var npos int64
	switch whence {
	case io.SeekStart:
		npos = offset
	case io.SeekCurrent:
		npos = int64(f.pos) + offset
	case io.SeekEnd:
		npos = int64(f.size) + offset
	default:
		return 0, fs.ErrInvalid
	}
	ipos := int(npos)
	if ipos < 0 || npos != int64(ipos) {
		return 0, fs.ErrInvalid
	}
	f.pos = ipos
	f.reader = nil
	return npos, nil
}

func (f *zfile) Stat() (fs.FileInfo, error) {
	return f, nil
}

type dir struct {
	name string
	pos  int
	list []string
	fsys *FileSystem
}

func (d *dir) Close() error {
	d.pos = -1
	return nil
}

func (d *dir) Read(p []byte) (n int, err error) {
	return 0, fs.ErrInvalid
}

func (d *dir) ReadDir(count int) ([]fs.DirEntry, error) {
	if d.pos < 0 {
		return nil, fs.ErrClosed
	}

	if count <= 0 {
		count = int(^uint(0) >> 1)
	} else if d.pos >= len(d.list) {
		return nil, io.EOF
	}

	var ret []fs.DirEntry
	for d.pos < len(d.list) && count > 0 {
		s, err := d.fsys.stat(d.list[d.pos])
		if err != nil {
			return ret, err
		}
		ret = append(ret, s)
		d.pos++
		count--
	}
	return ret, nil
}

func (d *dir) Stat() (fs.FileInfo, error) {
	return newDirInfo(d.name), nil
}

type dirInfo string

func newDirInfo(dir string) dirInfo          { return dirInfo(path.Base(dir)) }
func (d dirInfo) Name() string               { return string(d) }
func (d dirInfo) IsDir() bool                { return true }
func (d dirInfo) Type() fs.FileMode          { return fs.ModeDir }
func (d dirInfo) Info() (fs.FileInfo, error) { return d, nil }
func (d dirInfo) Size() int64                { return 0 }
func (d dirInfo) Mode() fs.FileMode          { return fs.ModeDir | 0555 }
func (d dirInfo) ModTime() time.Time         { return time.Time{} }
func (d dirInfo) Sys() interface{}           { return nil }

func getType(mimetype, name string, data []byte) string {
	if mimetype == "" {
		mimetype = mime.TypeByExtension(path.Ext(name))
	}
	if mimetype == "" {
		mimetype = http.DetectContentType(data)
	}
	return mimetype
}

func getHash(data []byte, isize int64) uint32 {
	if len(data) > 10+8 && data[0] == 0x1f && data[1] == 0x8b {
		if size := binary.LittleEndian.Uint32(data[len(data)-4:]); size == uint32(isize) {
			return binary.LittleEndian.Uint32(data[len(data)-8:])
		}
	}
	return 0
}

// Check interface implementations
var _ fs.ReadFileFS = &FileSystem{}
var _ fs.StatFS = &FileSystem{}
var _ fs.File = file{}
var _ fs.File = &zfile{}
var _ fs.ReadDirFile = &dir{}
var _ io.ReaderAt = file{}
var _ io.Seeker = file{}
var _ io.Seeker = &zfile{}
var _ entryInfo = object{}
var _ entryInfo = dirInfo("")

type entryInfo interface {
	fs.FileInfo
	fs.DirEntry
}
