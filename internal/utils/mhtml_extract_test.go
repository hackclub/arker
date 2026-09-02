package utils

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestExtractMHTMLHTMLDecodesRootPart(t *testing.T) {
	html := `<html><head><meta property="og:title" content="Captured title"></head></html>`
	tests := []struct{ encoding, body string }{
		{"quoted-printable", strings.ReplaceAll(html, "=", "=3D")},
		{"base64", base64.StdEncoding.EncodeToString([]byte(html))},
	}
	for _, test := range tests {
		t.Run(test.encoding, func(t *testing.T) {
			mhtml := fmt.Sprintf("MIME-Version: 1.0\r\nContent-Type: multipart/related; boundary=\"root\"\r\n\r\n--root\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: %s\r\n\r\n%s\r\n--root\r\nContent-Type: image/png\r\n\r\nnot-needed\r\n--root--\r\n", test.encoding, test.body)
			got, err := ExtractMHTMLHTML(strings.NewReader(mhtml), 1024)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, []byte(html)) {
				t.Fatalf("HTML = %q, want %q", got, html)
			}
		})
	}
}

func TestExtractMHTMLHTMLEnforcesDecodedLimit(t *testing.T) {
	mhtml := "Content-Type: multipart/related; boundary=x\r\n\r\n--x\r\nContent-Type: text/html\r\n\r\n<html>too large</html>\r\n--x--\r\n"
	if _, err := ExtractMHTMLHTML(strings.NewReader(mhtml), 4); err == nil {
		t.Fatal("expected decoded HTML limit error")
	}
}
