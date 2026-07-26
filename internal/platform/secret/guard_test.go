package secret_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardedTrees are the packages that build responses. Nothing under them may
// unwrap a Secret.
var guardedTrees = []string{
	filepath.Join("..", "..", "api"),
	filepath.Join("..", "..", "subplane"),
}

// TestRevealIsAbsentFromResponseBuilders is the primary defence for the
// out-of-band credential rule (§12.1). The type system is the backstop; this
// is the check that fails the build.
//
// Reveal() is legitimate in the mailer, the Telegram notifier and the config
// writer -- every one of which sends out of band. It has no business anywhere
// that writes an HTTP response, and the difference is not something a code
// review reliably catches on the hundredth pull request.
func TestRevealIsAbsentFromResponseBuilders(t *testing.T) {
	for _, tree := range guardedTrees {
		if _, err := os.Stat(tree); os.IsNotExist(err) {
			// The package has not been written yet. The guard starts applying
			// the moment it exists.
			continue
		}

		err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}

			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(src), "\n") {
				code, _, _ := strings.Cut(line, "//")
				if strings.Contains(code, ".Reveal()") {
					t.Errorf("%s:%d unwraps a Secret inside a response builder:\n\t%s",
						path, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
}
