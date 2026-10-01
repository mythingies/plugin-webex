package webex

import (
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

func TestMultipartBody(t *testing.T) {
	body, contentType, err := multipartBody([][2]string{{"roomId", "r1"}, {"markdown", "**hi**\nthere"}}, "SKILL.md", []byte("---\nname: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("content type %q: %v", contentType, err)
	}
	r := multipart.NewReader(body, params["boundary"])
	got := map[string]string{}
	var fileName string
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		got[p.FormName()] = string(data)
		if p.FormName() == "files" {
			fileName = p.FileName()
		}
	}
	if got["roomId"] != "r1" || got["markdown"] != "**hi**\nthere" || got["files"] != "---\nname: x\n" || fileName != "SKILL.md" {
		t.Errorf("parts = %q, file name %q", got, fileName)
	}
}
