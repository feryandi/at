package theme

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed auth.css
var authCSS []byte

// WriteFiles writes the bundled portal theme files to {dataDir}/theme/.
// Returns the absolute path to auth.css for use in log output.
// Called on every startup so the file stays in sync with the embedded version.
func WriteFiles(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "theme")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	cssPath := filepath.Join(dir, "auth.css")
	if err := os.WriteFile(cssPath, authCSS, 0644); err != nil {
		return "", err
	}
	return cssPath, nil
}
