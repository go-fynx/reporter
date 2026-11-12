package reporter

import "testing"

func TestFuncPackage(t *testing.T) {
	tests := map[string]string{
		"main.main":                                 "main",
		"main.main.func1":                           "main",
		"github.com/acme/app/db.(*Store).Get":       "github.com/acme/app/db",
		"github.com/acme/app/db.Get[...]":           "github.com/acme/app/db",
		"net/http.(*conn).serve":                    "net/http",
		"gopkg.in/yaml%2ev3.Unmarshal":              "gopkg.in/yaml.v3",
		"internal/runtime/maps.fatal":               "internal/runtime/maps",
		"github.com/go-fynx/reporter/reporter-go.F": "github.com/go-fynx/reporter/reporter-go",
	}
	for fn, want := range tests {
		if got := funcPackage(fn); got != want {
			t.Errorf("funcPackage(%q) = %q, want %q", fn, got, want)
		}
	}
}

func TestInProject(t *testing.T) {
	patterns := []string{"main*", "github.com/acme/app/**", "github.com/acme/lib/*"}
	for pkg, want := range map[string]bool{
		"main":                        true,
		"github.com/acme/app":         true,
		"github.com/acme/app/db/sql":  true,
		"github.com/acme/application": false,
		"github.com/acme/lib/x":       true,
		"github.com/acme/lib/x/y":     false,
		"net/http":                    false,
	} {
		if got := inProject(pkg, patterns); got != want {
			t.Errorf("inProject(%q) = %v, want %v", pkg, got, want)
		}
	}
}

func TestTrimPath(t *testing.T) {
	tests := []struct {
		file, pkg, root, want string
	}{
		{"/home/u/proj/main.go", "main", "/home/u/proj", "main.go"},
		{"/home/u/proj/main.go", "main", "", "/home/u/proj/main.go"},
		{"/home/u/go/pkg/mod/github.com/x/y@v1.2.0/z.go", "github.com/x/y", "", "github.com/x/y@v1.2.0/z.go"},
		{"/usr/local/go/src/net/http/server.go", "net/http", "", "net/http/server.go"},
		{"github.com/acme/app/db/db.go", "github.com/acme/app/db", "", "github.com/acme/app/db/db.go"},
	}
	for _, tt := range tests {
		if got := trimPath(tt.file, tt.pkg, tt.root); got != tt.want {
			t.Errorf("trimPath(%q, %q, %q) = %q, want %q", tt.file, tt.pkg, tt.root, got, tt.want)
		}
	}
}
