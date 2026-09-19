package gateway_test

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestGatewayDoesNotImportCompute(t *testing.T) {
	t.Parallel()
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedImports | packages.NeedModule,
	}, "github.com/jmylchreest/lobslaw/internal/gateway", "github.com/jmylchreest/lobslaw/internal/gateway/ui")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("packages.Load returned no packages")
	}
	for _, pkg := range pkgs {
		for path := range pkg.Imports {
			if path == "github.com/jmylchreest/lobslaw/internal/compute" ||
				strings.HasPrefix(path, "github.com/jmylchreest/lobslaw/internal/compute/") {
				t.Errorf("%s imports %s — transport/auth must not depend on the concrete agent", pkg.PkgPath, path)
			}
		}
	}
}
