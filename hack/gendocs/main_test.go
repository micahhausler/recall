package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDocsAreGenerated(t *testing.T) {
	docs, err := render("../..")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range docs {
		got, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from what hack/gendocs renders; run `go generate ./hack/gendocs`", name)
		}
	}
}
