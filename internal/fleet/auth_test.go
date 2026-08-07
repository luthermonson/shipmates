package fleet

import "testing"

func TestModuleAssetsArePublic(t *testing.T) {
	for _, path := range []string{"/app.js", "/api.js", "/utils.js"} {
		if !publicPaths[path] {
			t.Fatalf("module asset %s is not public", path)
		}
	}
}
