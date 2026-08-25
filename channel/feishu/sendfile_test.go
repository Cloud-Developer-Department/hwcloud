package feishu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

func TestSendFileParams(t *testing.T) {
	def := (&SendFile{}).Definition()
	if def.Name != "feishu_sendfile" {
		t.Fatalf("tool name = %q, want %q", def.Name, "feishu_sendfile")
	}
	if def.Parameters == nil {
		t.Fatal("parameters should not be nil")
	}
}

func TestReceiveFromContext(t *testing.T) {
	t.Run("with metadata", func(t *testing.T) {
		ctx := hwcloud.WithSession(context.Background(), hwcloud.Session{
			Metadata: map[string]any{
				"feishu_receive_id_type": "chat_id",
				"feishu_receive_id":      "oc_123",
			},
		})
		rit, rid, ok := receiveFromContext(ctx)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if rit != "chat_id" {
			t.Errorf("receiveIDType = %q, want %q", rit, "chat_id")
		}
		if rid != "oc_123" {
			t.Errorf("receiveID = %q, want %q", rid, "oc_123")
		}
	})

	t.Run("without session", func(t *testing.T) {
		_, _, ok := receiveFromContext(context.Background())
		if ok {
			t.Fatal("expected ok=false for bare context")
		}
	})

	t.Run("with session but no metadata", func(t *testing.T) {
		ctx := hwcloud.WithSession(context.Background(), hwcloud.Session{})
		_, _, ok := receiveFromContext(ctx)
		if ok {
			t.Fatal("expected ok=false for session without metadata")
		}
	})
}

func TestSendFileExecute_FileNotFound(t *testing.T) {
	dir := t.TempDir()
	sf := NewSendFile(&Channel{}, dir)
	args, _ := json.Marshal(SendFileParams{Path: "nonexistent.txt"})
	result := sf.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestSendFileExecute_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	os.WriteFile(path, []byte{}, 0644)

	sf := NewSendFile(&Channel{}, dir)
	args, _ := json.Marshal(SendFileParams{Path: "empty.txt"})
	result := sf.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("expected error for empty file")
	}
}

func TestSendFileExecute_Directory(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "subdir")
	os.MkdirAll(subdir, 0755)

	sf := NewSendFile(&Channel{}, dir)
	args, _ := json.Marshal(SendFileParams{Path: "subdir"})
	result := sf.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("expected error for directory")
	}
}

func TestSendFileExecute_NoChatContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello"), 0644)

	sf := NewSendFile(&Channel{}, dir)
	args, _ := json.Marshal(SendFileParams{Path: "test.txt"})
	result := sf.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("expected error when no Feishu chat context")
	}
}

func TestImageExtsAndFileTypeForExt(t *testing.T) {
	cases := []struct {
		ext      string
		isImage  bool
		fileType string
	}{
		{".jpg", true, ""},
		{".png", true, ""},
		{".gif", true, ""},
		{".pdf", false, "pdf"},
		{".doc", false, "doc"},
		{".docx", false, "doc"},
		{".xls", false, "xls"},
		{".xlsx", false, "xls"},
		{".ppt", false, "ppt"},
		{".pptx", false, "ppt"},
		{".mp4", false, "mp4"},
		{".opus", false, "opus"},
		{".txt", false, "stream"},
		{".zip", false, "stream"},
	}
	for _, c := range cases {
		gotImage := imageExts[c.ext]
		if gotImage != c.isImage {
			t.Errorf("imageExts[%q] = %v, want %v", c.ext, gotImage, c.isImage)
		}
		if !c.isImage {
			gotType := fileTypeForExt(c.ext)
			if gotType != c.fileType {
				t.Errorf("fileTypeForExt(%q) = %q, want %q", c.ext, gotType, c.fileType)
			}
		}
	}
}
