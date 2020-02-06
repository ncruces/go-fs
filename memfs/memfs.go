// Package memfs implements an in memory http.FileSystem.
//
// The filesystem can be statically generated, or loaded (and modified) at runtime.
// It is safe for concurrent reads (not writes), and biased towards read performance.
//
// File names should be slash separated, and rooted (start with a slash).
// Directories are implicit.
// Files can be gzip-compressed in memory.
// Methods are provided to serve gziped content directly to accepting HTTP clients.
//
// Usage:
//	assets, err = memfs.LoadCompressed(http.Dir("static"), gzip.BestCompression)
//	if err != nil {
//		log.Fatal(err)
//	}
//	log.Fatal(http.ListenAndServe("localhost:http", fs))
package memfs

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// FileSystem is the in memory http.FileSystem implementation.
type FileSystem struct {
	objs map[string]object
	dirs map[string][]string
}

// Create creates an empty FileSystem instance.
func Create() *FileSystem {
	return &FileSystem{
		objs: map[string]object{},
		dirs: map[string][]string{"/": nil},
	}
}

// Load loads the contents of an http.FileSystem into a new FileSystem instance.
func Load(in http.FileSystem) (*FileSystem, error) {
	return LoadCompressed(in, gzip.NoCompression)
}

// LoadCompressed loads the contents of an http.FileSystem into a new FileSystem instance.
// Files are gzip-compressed with the specified compression level.
func LoadCompressed(in http.FileSystem, level int) (*FileSystem, error) {
	fs := Create()
	if err := fs.load(in, "/", level); err != nil {
		return nil, err
	}
	return fs, nil
}

func (fs *FileSystem) load(in http.FileSystem, name string, level int) error {
	dir, err := in.Open(name)
	if err != nil {
		return err
	}
	chldn, err := dir.Readdir(0)
	if err != nil {
		return err
	}
	for _, c := range chldn {
		name := path.Join(name, c.Name())
		if c.IsDir() {
			err = fs.load(in, name, level)
		} else {
			var file http.File
			if file, err = in.Open(name); err == nil {
				err = fs.CreateCompressed(name, "", c.ModTime(), file, level)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Open implements http.FileSystem, opening files for reading.
// Compressed files are decompressed on-the-fly.
// Seeking compressed files is emulated and can be extremely slow.
func (fs *FileSystem) Open(name string) (http.File, error) {
	if o, ok := fs.objs[name]; ok {
		return &file{object: o}, nil
	}
	if d, ok := fs.dirs[name]; ok {
		return &dir{name: name, list: d, fs: fs}, nil
	}
	return nil, os.ErrNotExist
}

// Stat is a shortcut for fs.Open(name).Stat().
func (fs *FileSystem) Stat(name string) (os.FileInfo, error) {
	if o, ok := fs.objs[name]; ok {
		return o, nil
	}
	if _, ok := fs.dirs[name]; ok {
		return statDir(name), nil
	}
	return nil, os.ErrNotExist
}

// Create creates a file.
// Overwrites an existing file (but not a directory).
// Sniffs the MIME type if none is provided.
func (fs *FileSystem) Create(name, mimetype string, modtime time.Time, content io.ReadSeeker) error {
	if !strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return os.ErrInvalid
	}
	if _, ok := fs.dirs[name]; ok {
		return os.ErrExist
	}

	mimetype, err := getType(name, mimetype, content)
	if err != nil {
		return err
	}
	n, err := getSize(content)
	if err != nil {
		return err
	}

	var hash = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	var buf strings.Builder
	buf.Grow(int(n))

	n, err = io.Copy(io.MultiWriter(&buf, hash), content)
	if err == nil {
		fs.put(name, object{
			size: int(n),
			time: modtime,
			mime: mimetype,
			data: buf.String(),
			hash: hash.Sum32(),
		}, false)
	}
	return err
}

// CreateCompressed creates a compressed file.
// Overwrites an existing file (but not a directory).
// Files are gzip-compressed with the specified compression level.
// Sniffs the MIME type if none is provided.
func (fs *FileSystem) CreateCompressed(name, mimetype string, modtime time.Time, content io.ReadSeeker, level int) error {
	if level == gzip.NoCompression {
		return fs.Create(name, mimetype, modtime, content)
	}
	if !strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return os.ErrInvalid
	}
	if _, ok := fs.dirs[name]; ok {
		return os.ErrExist
	}

	mimetype, err := getType(name, mimetype, content)
	if err != nil {
		return err
	}
	n, err := getSize(content)
	if err != nil {
		return err
	}
	if n < 1024 {
		return fs.Create(name, mimetype, modtime, content)
	}

	var buf bytes.Buffer
	buf.Grow(int(n))

	gzip, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return err
	}
	defer gzip.Close()
	gzip.ModTime = modtime

	n, err = io.Copy(gzip, content)
	if err == nil {
		err = gzip.Close()
	}
	if err == nil && 4*n >= 5*int64(buf.Len()) {
		fs.put(name, object{
			size: int(n),
			time: modtime,
			mime: mimetype,
			data: buf.String(),
			hash: getHash(buf.Bytes(), n),
		}, false)
		return nil
	}

	_, err = content.Seek(0, io.SeekStart)
	if err == nil {
		return fs.Create(name, mimetype, modtime, content)
	}
	return err
}

// CreateString creates a file from a string.
// This intended to be used by code generators.
// Bad things happen if you violate its expectations.
//
// Overwrites an existing file.
// Files are expected to be passed in filepath.Walk order.
// MIME type will NOT be sniffed and content will NOT be compressed.
// If size != len(content), content is assumed to be gzip-compressed, and size its uncompressed size.
func (fs *FileSystem) CreateString(name, mimetype string, modtime time.Time, hash uint32, size int, content string) {
	fs.put(name, object{
		size: size,
		time: modtime,
		mime: mimetype,
		data: content,
		hash: hash,
	}, true)
}

func (fs *FileSystem) put(name string, obj object, ordered bool) {
	dir, file := path.Split(name)
	obj.name = file
	fs.objs[name] = obj

	var hasFile = func(dir []string, name string) bool {
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

	var addFile = func(dir string, file string) bool {
		d := fs.dirs[dir]
		if hasFile(d, file) {
			return false
		}
		fs.dirs[dir] = append(d, file)
		return true
	}

	for len(dir) > 1 { // dir != "/"
		// remove trailing slash
		dir = dir[:len(dir)-1]
		if addFile(dir, name) {
			// continue with parent
			name = dir
			dir, _ = path.Split(dir)
		} else {
			return
		}
	}
	addFile(dir, name)
}

type object struct {
	name string
	data string
	size int
	time time.Time
	mime string
	hash uint32
}

func (o object) Name() string       { return o.name }
func (o object) Size() int64        { return int64(o.size) }
func (o object) Mode() os.FileMode  { return 0444 }
func (o object) ModTime() time.Time { return o.time }
func (o object) IsDir() bool        { return false }
func (o object) Sys() interface{}   { return nil }

type file struct {
	object
	pos    int
	reader io.ReadCloser
}

func (f *file) Close() error {
	f.pos = -1
	if f.reader != nil {
		return f.reader.Close()
	}
	return nil
}

func (f *file) Read(p []byte) (n int, err error) {
	if f.pos < 0 {
		return 0, os.ErrClosed
	}
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if f.reader == nil {
		if len(f.data) == f.size {
			f.reader = ioutil.NopCloser(strings.NewReader(f.data[f.pos:]))
		} else {
			f.reader, err = gzip.NewReader(strings.NewReader(f.data))
			if err == nil && f.pos > 0 {
				var n64 int64
				n64, err = io.CopyN(ioutil.Discard, f.reader, int64(f.pos))
				n = int(n64)
				f.pos = n
			}
			if err != nil {
				return
			}
		}
	}
	n, err = f.reader.Read(p)
	f.pos += n
	return
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	if f.pos < 0 {
		return 0, os.ErrClosed
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
		return 0, os.ErrInvalid
	}
	ipos := int(npos)
	if ipos < 0 || npos != int64(ipos) {
		return 0, os.ErrInvalid
	}
	f.pos = ipos
	f.reader = nil
	return npos, nil
}

func (f *file) Readdir(count int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (f *file) Stat() (os.FileInfo, error) {
	return f, nil
}

type dir struct {
	name string
	pos  int
	list []string
	fs   *FileSystem
}

func (d *dir) Close() error {
	d.pos = -1
	return nil
}

func (d *dir) Read(p []byte) (n int, err error) {
	if d.pos < 0 {
		return 0, os.ErrClosed
	} else {
		return 0, io.EOF
	}
}

func (d *dir) Seek(offset int64, whence int) (int64, error) {
	return 0, os.ErrInvalid
}

func (d *dir) Readdir(count int) ([]os.FileInfo, error) {
	if d.pos < 0 {
		return nil, os.ErrClosed
	}

	if count <= 0 {
		count = int(^uint(0) >> 1)
	} else if d.pos >= len(d.list) {
		return nil, io.EOF
	}

	var ret []os.FileInfo
	for d.pos < len(d.list) && count > 0 {
		s, err := d.fs.Stat(d.list[d.pos])
		if err != nil {
			return ret, err
		}
		ret = append(ret, s)
		d.pos++
		count--
	}
	return ret, nil
}

func (d *dir) Stat() (os.FileInfo, error) {
	return statDir(d.name), nil
}

type dirStat string

func statDir(dir string) dirStat     { return dirStat(path.Base(dir)) }
func (d dirStat) Name() string       { return string(d) }
func (d dirStat) Size() int64        { return 0 }
func (d dirStat) Mode() os.FileMode  { return os.ModeDir | 0555 }
func (d dirStat) ModTime() time.Time { return time.Time{} }
func (d dirStat) IsDir() bool        { return true }
func (d dirStat) Sys() interface{}   { return nil }

// ServeHTTP implements http.Handler using ServeFile.
// Replaces http.FileServer.
func (fs *FileSystem) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fs.ServeFile(w, r, r.URL.Path)
}

// ServeFile replaces http.ServeFile.
// Redirects to canonical paths.
// Serves index.html for directories, 404.html for not found.
// Doesn't list directories.
func (fs *FileSystem) ServeFile(w http.ResponseWriter, r *http.Request, name string) {
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	r.URL.Path = name
	fs.serveFile(w, r, path.Clean(name))
}

// ServeContent replaces http.ServeContent.
// Serves the named file.
// No redirects or rewrites.
func (fs *FileSystem) ServeContent(w http.ResponseWriter, r *http.Request, name string) {
	if o, ok := fs.objs[name]; ok {
		if setHeaders(w, r, &o) {
			http.ServeContent(w, r, o.name, o.time, strings.NewReader(o.data))
			return
		}
		http.ServeContent(w, r, o.name, o.time, &file{object: o})
	} else {
		http.NotFound(w, r)
	}
}

func (fs *FileSystem) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := fs.dirs[name]; ok {
		name = strings.TrimSuffix(name, "/") + "/index.html"
	}
	if o, ok := fs.objs[name]; ok && name != "/404.html" {
		if setHeaders(w, r, &o) {
			http.FileServer(rawFileSystem{fs}).ServeHTTP(w, r)
			return
		}
		http.FileServer(fs).ServeHTTP(w, r)
	} else {
		fs.notFound(w, r)
	}
}

func (fs *FileSystem) notFound(w http.ResponseWriter, r *http.Request) {
	if o, ok := fs.objs["/404.html"]; ok {
		o.mime = "text/html; charset=utf-8"
		o.hash = 0

		var stream io.ReadSeeker
		if setHeaders(w, r, &o) {
			stream = strings.NewReader(o.data)
		} else {
			stream = &file{object: o}
		}
		if r.Method != "HEAD" {
			w.WriteHeader(http.StatusNotFound)
			io.Copy(w, stream)
		}
	} else {
		http.NotFound(w, r)
	}
}

func setHeaders(w http.ResponseWriter, r *http.Request, o *object) (gzip bool) {
	header := w.Header()
	if len(o.data) != o.size {
		header.Add("Vary", "Accept-Encoding")
		if httpguts.HeaderValuesContainsToken(r.Header["Accept-Encoding"], "gzip") {
			header.Set("Content-Encoding", "gzip")
			gzip = true
		}
	}
	if o.mime != "" {
		header.Set("Content-Type", o.mime)
	}
	if o.hash != 0 {
		if tag := strconv.FormatUint(uint64(o.hash), 36); gzip {
			header.Set("ETag", `W/"`+tag+`"`)
		} else {
			header.Set("ETag", `"`+tag+`"`)
		}
	}
	return
}

// Serve raw gzipped objects
type rawFileSystem struct {
	*FileSystem
}

func (fs rawFileSystem) Open(name string) (http.File, error) {
	if o, ok := fs.objs[name]; ok {
		return rawFile{strings.NewReader(o.data), o}, nil
	}
	if d, ok := fs.dirs[name]; ok {
		return &dir{name: name, list: d, fs: fs.FileSystem}, nil
	}
	return nil, os.ErrNotExist
}

type rawFile struct {
	*strings.Reader
	object
}

func (f rawFile) Size() int64 {
	return int64(f.Len())
}

func (f rawFile) Close() error {
	return nil
}

func (f rawFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (f rawFile) Stat() (os.FileInfo, error) {
	return f, nil
}

func getType(name, mimetype string, content io.ReadSeeker) (string, error) {
	if mimetype == "" && mime.TypeByExtension(path.Ext(name)) == "" {
		var buf [512]byte
		n, _ := io.ReadFull(content, buf[:])
		mimetype = http.DetectContentType(buf[:n])
		_, err := content.Seek(0, io.SeekStart)
		if err != nil {
			return "", err
		}
	}
	return mimetype, nil
}

func getSize(seeker io.Seeker) (n int64, err error) {
	n, err = seeker.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = seeker.Seek(0, io.SeekStart)
	}
	return
}

func getHash(data []byte, isize int64) uint32 {
	if len(data) > 10+8 && data[0] == 0x1f && data[1] == 0x8b {
		if size := binary.LittleEndian.Uint32(data[len(data)-4:]); size == uint32(isize) {
			return binary.LittleEndian.Uint32(data[len(data)-8:])
		}
	}
	return 0
}
