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
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// FileSystem is the in memory http.FileSystem implementation.
type FileSystem struct {
	objs map[string]object
	dirs map[string][]string
}

// Creates an empty FileSystem instance.
func Create() *FileSystem {
	return &FileSystem{
		objs: map[string]object{},
		dirs: map[string][]string{"/": nil},
	}
}

// Loads the contents of an http.FileSystem into a new FileSystem instance.
func Load(in http.FileSystem) (*FileSystem, error) {
	return LoadCompressed(in, gzip.NoCompression)
}

// Loads the contents of an http.FileSystem into a new FileSystem instance.
// Files are compressed with the specified gzip compression level.
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
// Seeking support is limited for compressed files: they can be seeked, but subsequent reads might fail.
// Rewinding to the start is supported for all files.
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
	if d, ok := fs.dirs[name]; ok {
		return &dir{name: name, list: d, fs: fs}, nil
	}
	return nil, os.ErrNotExist
}

// Creates a file. Overwrites an existing file (but not a directory).
// Sniffs the MIME type if one is not provided.
func (fs *FileSystem) Create(name, mimetype string, modtime time.Time, content io.ReadSeeker) error {
	if _, ok := fs.dirs[name]; ok {
		return os.ErrExist
	}

	mimetype, err := sniffType(name, mimetype, content)
	if err != nil {
		return err
	}
	n, err := seekerLen(content)
	if err != nil {
		return err
	}

	var builder strings.Builder
	builder.Grow(int(n))

	n, err = io.Copy(&builder, content)
	if err == nil {
		fs.put(name, object{
			size: int(n),
			time: modtime,
			mime: mimetype,
			data: builder.String(),
		})
	}
	return err
}

// Creates a compressed file. Overwrites an existing file (but not a directory).
// Files are compressed with the specified gzip compression level.
// Sniffs the MIME type if one is not provided.
func (fs *FileSystem) CreateCompressed(name, mimetype string, modtime time.Time, content io.ReadSeeker, level int) error {
	if level == gzip.NoCompression {
		return fs.Create(name, mimetype, modtime, content)
	}
	if _, ok := fs.dirs[name]; ok {
		return os.ErrExist
	}

	mimetype, err := sniffType(name, mimetype, content)
	if err != nil {
		return err
	}

	var builder strings.Builder

	gzip, err := gzip.NewWriterLevel(&builder, level)
	if err != nil {
		return err
	}
	defer gzip.Close()

	n, err := io.Copy(gzip, content)
	if err == nil {
		err = gzip.Flush()
	}
	if err == nil && 4*n >= 5*int64(builder.Len()) {
		fs.put(name, object{
			size: int(n),
			time: modtime,
			mime: mimetype,
			data: builder.String(),
		})
		return nil
	}

	_, err = content.Seek(0, io.SeekStart)
	if err == nil {
		return fs.Create(name, mimetype, modtime, content)
	}
	return err
}

// Creates a file from a string. This intended to be used by code generators.
// Overwrites an existing file (but panics if it's a directory).
// MIME type will NOT be sniffed and content will NOT be compressed.
// If content is already gzipped (but MIME type and extension don't match that) it'll get decompressed on-the-fly.
// You're strongly encouraged to provide a MIME type, particularly for compressed files.
func (fs *FileSystem) CreateString(name, mimetype string, modtime time.Time, content string) {
	if _, ok := fs.dirs[name]; ok {
		panic(os.ErrExist)
	}

	size := len(content)
	if len(content) >= 10+8 &&
		// check for gzipped content
		strings.HasPrefix(content, "\x1f\x8b") &&
		// which should not be gzipped content
		mimetype != "application/gzip" &&
		mimetype != "application/x-gzip" &&
		path.Ext(name) != ".gz" {
		// get the uncompressed length
		size = int(binary.LittleEndian.Uint32([]byte(content[size-4:])))
		// it'll get decompressed on-the-fly
	}

	fs.put(name, object{
		size: size,
		time: modtime,
		mime: mimetype,
		data: content,
	})
}

func (fs *FileSystem) put(name string, obj object) {
	dir, file := path.Split(name)
	obj.name = file
	fs.objs[name] = obj

	contains := func(slice []string, str string) bool {
		for i := len(slice) - 1; i >= 0; i-- {
			if slice[i] == str {
				return true
			}
		}
		return false
	}

	for {
		if len(dir) > 1 {
			dir = dir[:len(dir)-1]
		}
		if d := fs.dirs[dir]; !contains(d, name) {
			fs.dirs[dir] = append(d, name)
		} else {
			return
		}
		if len(dir) > 1 {
			name = dir
			dir, _ = path.Split(dir)
		} else {
			return
		}
	}
}

type object struct {
	name string
	data string
	size int
	time time.Time
	mime string
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
			if f.pos > 0 {
				return 0, errors.New("read after seek in compressed file")
			}
			f.reader, err = gzip.NewReader(strings.NewReader(f.data))
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
	return d, nil
}

func (d *dir) Name() string       { return d.name[strings.LastIndexByte(d.name, '/')+1:] }
func (d *dir) Size() int64        { return 0 }
func (d *dir) Mode() os.FileMode  { return os.ModeDir | 0555 }
func (d *dir) ModTime() time.Time { return time.Time{} }
func (d *dir) IsDir() bool        { return true }
func (d *dir) Sys() interface{}   { return nil }

// ServeHTTP implements http.Handler, replacing http.FileServer.
func (fs *FileSystem) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
		r.URL.Path = name
	}
	fs.serveFile(w, r, path.Clean(name))
}

// Like http.ServeFile.
func (fs *FileSystem) ServeFile(w http.ResponseWriter, r *http.Request, name string) {
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	r.URL.Path = name
	fs.serveFile(w, r, path.Clean(name))
}

// Like http.ServeContent.
func (fs *FileSystem) ServeContent(w http.ResponseWriter, r *http.Request, name string) {
	if o, ok := fs.objs[name]; ok {
		header := w.Header()
		if o.mime != "" {
			header.Set("Content-Type", o.mime)
		}
		if len(o.data) != o.size {
			header.Add("Vary", "Accept-Encoding")
			if httpguts.HeaderValuesContainsToken(r.Header["Accept-Encoding"], "gzip") {
				header.Add("Content-Encoding", "gzip")
				http.ServeContent(w, r, o.name, o.time, strings.NewReader(o.data))
				return
			}
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
	if o, ok := fs.objs[name]; ok {
		header := w.Header()
		if o.mime != "" {
			header.Set("Content-Type", o.mime)
		}
		if len(o.data) != o.size {
			header.Add("Vary", "Accept-Encoding")
			if httpguts.HeaderValuesContainsToken(r.Header["Accept-Encoding"], "gzip") {
				header.Add("Content-Encoding", "gzip")
				http.FileServer(rawFileSystem{fs}).ServeHTTP(w, r)
				return
			}
		}
	}

	http.FileServer(fs).ServeHTTP(w, r)
}

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
	io.ReadSeeker
	os.FileInfo
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

func sniffType(name, mimetype string, content io.ReadSeeker) (string, error) {
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

func seekerLen(seeker io.Seeker) (n int64, err error) {
	n, err = seeker.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = seeker.Seek(0, io.SeekStart)
	}
	return
}
