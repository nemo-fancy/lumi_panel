package events

import (
	"io"
	"log/slog"
)

// discardLogger keeps expected-failure tests from writing noise to stderr.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
