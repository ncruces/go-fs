package main

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"go/token"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/gabriel-vasile/mimetype"
)

type Assets struct {
	Package  string
	Variable string
	Assets   <-chan Asset
}

type Asset struct {
	Name  string
	Type  string
	Time  int64
	Lines <-chan string
}

func main() {
	varName := flag.String("var", "assets", "variable name")
	pkgName := flag.String("pkg", "", "package name (default directory name of <target-file>)")
	flag.Var(mimeTypes, "mimetype", "register a MIME type (e.g. png:image/png)")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 2 {
		usage()
	}

	source := flag.Arg(0)
	target := flag.Arg(1)

	// check that source exists and is a directory
	if s, err := os.Stat(source); os.IsNotExist(err) {
		fatal("source-dir %s: does not exist", source)
	} else if err != nil {
		fatal("source-dir %s: %v", source, err)
	} else if !s.IsDir() {
		fatal("source-dir %s: not a directory", source)
	}

	// check that target is a go file
	if filepath.Ext(target) != ".go" {
		fatal("target-file %s: not a .go file", target)
	}
	// create the target directory
	tgtdir := filepath.Dir(target)
	if err := os.MkdirAll(tgtdir, 0755); err != nil {
		fatal("target-file %s: %v", target, err)
	}
	// package name defaults to directory name
	if *pkgName == "" {
		if s, err := os.Stat(tgtdir); err != nil {
			fatal("target-file %s: %v", target, err)
		} else {
			*pkgName = s.Name()
		}
	}

	// identifiers should be valid
	if !token.IsIdentifier(*pkgName) {
		fatal("invalid package name: %s", *varName)
	}
	if !token.IsIdentifier(*varName) {
		fatal("invalid variable name: %s", *varName)
	}

	// create target
	out, err := os.Create(target)
	if err != nil {
		fatal("target-file %s: %v", target, err)
	}
	defer out.Close()

	assets := make(chan Asset)
	go walk(source, assets)

	tmpl := template.Must(template.ParseFiles("template.gogo"))
	if err := tmpl.Execute(out, Assets{*pkgName, *varName, assets}); err != nil {
		fatal("generating output: %v", err)
	}
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options] <source-dir> <target-file>\n", filepath.Base(os.Args[0]))
	flag.PrintDefaults()
	os.Exit(2)
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(flag.CommandLine.Output(), format+"\n", a...)
	os.Exit(2)
}

func walk(root string, assets chan<- Asset) {
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			data, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}

			path, err = filepath.Rel(root, path)
			if err != nil {
				return err
			}
			path = "/" + filepath.ToSlash(path)

			modtime := info.ModTime()
			lines := make(chan string)
			assets <- Asset{path, sniff(path, data), modtime.Unix(), lines}
			err = dump(compress(data, modtime), lines)
			close(lines)
			return err
		}
		return err
	})
	close(assets)
}

func dump(data []byte, lines chan<- string) error {
	var line strings.Builder
	var char = []byte(`\xXX`)
	for i := 0; i < len(data); {
		for line.Len() < 80 && i < len(data) {
			hex.Encode(char[2:], data[i:i+1])
			line.Write(char)
			i++
		}
		lines <- line.String()
		line.Reset()
	}

	return nil
}

func compress(data []byte, modtime time.Time) []byte {
	if len(data) < 24 {
		return data
	}

	var buf bytes.Buffer

	gzip, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	defer gzip.Close()

	gzip.ModTime = modtime

	_, err := gzip.Write(data)
	if err == nil {
		err = gzip.Close()
	}
	if err == nil && 4*len(data) >= 5*buf.Len() {
		return buf.Bytes()
	}

	return data
}

type MimeTypes struct {
	m map[string]string
}

func (mt MimeTypes) String() string {
	return "mimetypes"
}

func (mt MimeTypes) Set(s string) error {
	split := strings.SplitN(s, ":", 2)
	if len(split) == 2 {
		_, _, err := mime.ParseMediaType(split[1])
		if err == nil {
			ext := "." + strings.ToLower(split[0])
			mt.m[ext] = split[1]
		}
		return err
	}
	return errors.New("expected colon after extension")
}

var mimeTypes = MimeTypes{map[string]string{
	".css":  "text/css; charset=utf-8",
	".gif":  "image/gif",
	".htm":  "text/html; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".js":   "application/javascript",
	".mjs":  "application/javascript",
	".pdf":  "application/pdf",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".wasm": "application/wasm",
	".webp": "image/webp",
	".xml":  "text/xml; charset=utf-8",
}}

func sniff(name string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ctype, ok := mimeTypes.m[ext]; ok {
		return ctype
	}

	ctype := http.DetectContentType(data)
	if ctype == "application/octet-stream" {
		ctype, _ = mimetype.Detect(data)
	}
	if ctype == "application/octet-stream" || ctype == "inode/x-empty" {
		ctype = ""
	}
	return ctype
}