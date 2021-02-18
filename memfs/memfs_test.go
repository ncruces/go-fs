package memfs_test

import (
	"compress/gzip"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ncruces/go-fs/memfs"
)

func TestCreate(t *testing.T) {
	fsys := memfs.Create()
	if err := fstest.TestFS(fsys); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	fsys, err := memfs.LoadCompressed(http.Dir("."), gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}

	if err := fstest.TestFS(fsys, "memfs.go", "memfs_test.go"); err != nil {
		t.Fatal(err)
	}
}

func TestFileSystem_Create(t *testing.T) {
	fsys := memfs.Create()

	if err := fsys.Create("hi.txt", "text/plain", time.Now(), strings.NewReader("Hello, world!")); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Create("dir/.keep", "", time.Now(), &strings.Reader{}); err != nil {
		t.Fatal(err)
	}

	if err := fstest.TestFS(fsys, "hi.txt"); err != nil {
		t.Fatal(err)
	}
}
