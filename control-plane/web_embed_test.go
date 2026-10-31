package controlplane

import (
	"io/fs"
	"testing"
)

func TestEmbeddedFrontend(t *testing.T) {
	for _, name := range []string{"web/index.html", "web/app.js", "web/styles.css"} {
		b, err := fs.ReadFile(Files, name)
		if err != nil || len(b) == 0 {
			t.Fatalf("embedded %s: len=%d err=%v", name, len(b), err)
		}
	}
}
