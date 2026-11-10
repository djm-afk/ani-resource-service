package main

import (
	"io"
	"log/slog"
	"testing"
)

func TestImageRuntimeConfigurationFailsClosed(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for _, key := range []string{"ANI_IMAGE_RUNTIME_ADDR", "ANI_IMAGE_RUNTIME_CA", "ANI_IMAGE_RUNTIME_CERT", "ANI_IMAGE_RUNTIME_KEY"} {
		t.Setenv(key, "")
	}
	if s, err := openImageRuntime(nil, logger); err != nil || s != nil {
		t.Fatal("disabled runtime opens listener", err)
	}
	t.Setenv("ANI_IMAGE_RUNTIME_CA", "/does/not/exist")
	if _, err := openImageRuntime(nil, logger); err == nil {
		t.Fatal("partial TLS configuration accepted")
	}
	t.Setenv("ANI_IMAGE_RUNTIME_ADDR", "127.0.0.1:19094")
	if _, err := openImageRuntime(nil, logger); err == nil {
		t.Fatal("runtime enabled without Image")
	}
}
