package memfs

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// ServeHTTP implements http.Handler using ServeFile.
// Replaces http.FileServer.
func (fsys *FileSystem) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// same transform as http.FileServer.ServeHTTP()
	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
		r.URL.Path = upath
	}
	upath = path.Clean(upath)

	// same transform as http.FS.Open()
	if upath == "/" {
		upath = "."
	} else {
		upath = upath[1:]
	}
	fsys.serveFile(w, r, upath)
}

// ServeFile replaces http.ServeFile.
// Redirects to canonical paths.
// Serves index.html for directories, 404.html for not found.
// Doesn't list directories.
func (fsys *FileSystem) ServeFile(w http.ResponseWriter, r *http.Request, name string) {
	if name == "." {
		r.URL.Path = "/"
	} else {
		r.URL.Path = "/" + name
	}
	fsys.serveFile(w, r, name)
}

// ServeContent replaces http.ServeContent.
// Serves the named file.
// No redirects or rewrites.
func (fs *FileSystem) ServeContent(w http.ResponseWriter, r *http.Request, name string) {
	if o, ok := fs.objs[name]; ok {
		var reader io.ReadSeeker
		if o.setHeaders(w, r) {
			reader = strings.NewReader(o.data)
		} else {
			reader = &zfile{object: o}
		}
		http.ServeContent(w, r, o.name, o.time, reader)
	} else {
		http.NotFound(w, r)
	}
}

func (fsys *FileSystem) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := fsys.dirs[name]; ok {
		if name == "." {
			name = "index.html"
		} else {
			name = name + "/index.html"
		}
	}
	if o, ok := fsys.objs[name]; ok && name != "404.html" {
		var fs fs.FS
		if o.setHeaders(w, r) {
			fs = rawFileSystem{fsys}
		} else {
			fs = fsys
		}
		http.FileServer(http.FS(fs)).ServeHTTP(w, r)
	} else {
		fsys.notFound(w, r)
	}
}

func (fsys *FileSystem) notFound(w http.ResponseWriter, r *http.Request) {
	if o, ok := fsys.objs["404.html"]; ok {
		o.mime = "text/html; charset=utf-8"
		o.hash = 0

		var reader io.ReadSeeker
		if o.setHeaders(w, r) {
			reader = strings.NewReader(o.data)
		} else {
			reader = &zfile{object: o}
		}
		w.WriteHeader(http.StatusNotFound)
		if r.Method != "HEAD" {
			io.Copy(w, reader)
		}
	} else {
		http.NotFound(w, r)
	}
}

func (o object) setHeaders(w http.ResponseWriter, r *http.Request) (raw bool) {
	raw = false
	weak := false
	header := w.Header()
	if len(o.data) == o.size {
		raw = true
	} else {
		header.Add("Vary", "Accept-Encoding")
		if httpguts.HeaderValuesContainsToken(r.Header["Accept-Encoding"], "gzip") {
			header.Set("Content-Encoding", "gzip")
			weak = true
			raw = true
		}
	}
	if o.mime != "" {
		header.Set("Content-Type", o.mime)
	}
	if o.hash != 0 {
		if tag := strconv.FormatUint(uint64(o.hash), 36); weak {
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

func (fsys rawFileSystem) Open(name string) (fs.File, error) {
	if o, ok := fsys.objs[name]; ok {
		return file{o, strings.NewReader(o.data)}, nil
	}
	if d, ok := fsys.dirs[name]; ok {
		return &dir{name: name, list: d, fsys: fsys.FileSystem}, nil
	}
	return nil, fs.ErrNotExist
}
