package wasmplugin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadWasmModuleLoadsHTTPPath(t *testing.T) {
	server := httptest.NewServer(http.FileServer(http.Dir("../wasmprocessor/testdata/nop")))
	defer server.Close()

	got, err := readWasmModule(t.Context(), server.URL+"/main.wasm")
	if err != nil {
		t.Fatalf("failed to read HTTP path: %v", err)
	}
	want, err := os.ReadFile("../wasmprocessor/testdata/nop/main.wasm")
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("HTTP path returned different wasm bytes")
	}
}

func TestReadWasmModuleLoadsFileURLPath(t *testing.T) {
	wasmPath, err := filepath.Abs("../wasmprocessor/testdata/nop/main.wasm")
	if err != nil {
		t.Fatalf("failed to resolve test wasm path: %v", err)
	}

	got, err := readWasmModule(t.Context(), (&url.URL{Scheme: "file", Path: wasmPath}).String())
	if err != nil {
		t.Fatalf("failed to read file URL path: %v", err)
	}
	want, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("file URL path returned different wasm bytes")
	}
}

func TestReadWasmModuleRejectsHostedFileURLPath(t *testing.T) {
	_, err := readWasmModule(t.Context(), "file://localhost/tmp/main.wasm")
	if err == nil {
		t.Fatal("expected hosted file URL to fail")
	}
	if !strings.Contains(err.Error(), "unsupported file URL host") {
		t.Fatalf("expected unsupported host error, got %v", err)
	}
}
