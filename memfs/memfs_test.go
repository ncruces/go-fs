package memfs

import (
	"testing"
	"testing/fstest"
)

func TestCreate(t *testing.T) {
	fs := Create()
	if err := fstest.TestFS(fs); err != nil {
		t.Fatal(err)
	}
}
