package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDocumentResolveDocumentFileFromWorkspacePath(t *testing.T) {
	workspace := t.TempDir()
	docPath := filepath.Join(workspace, "invoice.pdf")
	if err := os.WriteFile(docPath, []byte("%PDF-1.4\ninvoice"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := &ReadDocumentTool{}
	gotPath, gotMIME, err := tool.resolveDocumentFile(WithToolWorkspace(context.Background(), workspace), "", "invoice.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != docPath {
		t.Fatalf("path = %q, want %q", gotPath, docPath)
	}
	if gotMIME != "application/pdf" {
		t.Fatalf("mime = %q, want application/pdf", gotMIME)
	}
}

func TestReadDocumentResolveDocumentFileRejectsOutsideWorkspacePath(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := os.WriteFile(outside, []byte("%PDF-1.4\ninvoice"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := &ReadDocumentTool{}
	_, _, err := tool.resolveDocumentFile(WithToolWorkspace(context.Background(), workspace), "", outside)
	if err == nil {
		t.Fatal("expected outside-workspace path to fail")
	}
	if !strings.Contains(err.Error(), "invalid document path") {
		t.Fatalf("error = %v, want invalid document path", err)
	}
}
