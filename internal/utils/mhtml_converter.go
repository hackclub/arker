package utils

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"golang.org/x/net/html"
)

// MHTMLConverter handles MHTML to HTML conversion  
type MHTMLConverter struct{}

// PartData represents a multipart section
type PartData struct {
	Header map[string][]string
	Data   []byte
}

// Convert converts MHTML content to HTML
func (c *MHTMLConverter) Convert(input io.Reader, output io.Writer) error {
	// Read the input into memory first
	data, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	// Parse as mail message
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to read mail message: %w", err)
	}

	// Get content type and boundary
	contentType := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("failed to parse media type: %w", err)
	}

	if !strings.HasPrefix(mediaType, "multipart/related") {
		return fmt.Errorf("not a multipart/related message, got: %s", mediaType)
	}

	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("no boundary found in content type")
	}

	// Parse multipart content
	mr := multipart.NewReader(msg.Body, boundary)
	
	parts := make(map[string]*PartData)
	var htmlPart *PartData

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read multipart: %w", err)
		}

		partContentType := part.Header.Get("Content-Type")
		contentID := part.Header.Get("Content-ID")
		contentLocation := part.Header.Get("Content-Location")

		// Read part data
		partData, err := io.ReadAll(part)
		if err != nil {
			return fmt.Errorf("failed to read part data: %w", err)
		}

		pd := &PartData{
			Header: map[string][]string(part.Header),
			Data:   partData,
		}

		// Store by Content-ID
		if contentID != "" {
			cid := strings.Trim(contentID, "<>")
			parts[cid] = pd
		}

		// Store by Content-Location
		if contentLocation != "" {
			parts[contentLocation] = pd
			
			// Also store without "cid:" prefix for easier lookup
			if strings.HasPrefix(contentLocation, "cid:") {
				cidKey := strings.TrimPrefix(contentLocation, "cid:")
				parts[cidKey] = pd
			}
		}

		// Check if this is the HTML part
		if htmlPart == nil && strings.HasPrefix(partContentType, "text/html") {
			htmlPart = pd
		}
	}

	if htmlPart == nil {
		return fmt.Errorf("no HTML part found")
	}

	// Decode the HTML content
	htmlContent, err := c.decodePart(htmlPart)
	if err != nil {
		return fmt.Errorf("failed to decode HTML part: %w", err)
	}

	// Parse and modify HTML to embed resources
	doc, err := html.Parse(bytes.NewReader(htmlContent))
	if err != nil {
		return fmt.Errorf("failed to parse HTML: %w", err)
	}

	// Walk the HTML tree and replace cid: references
	c.walkHTML(doc, parts)

	// Render the modified HTML
	return html.Render(output, doc)
}

// decodePart decodes a multipart based on its transfer encoding
func (c *MHTMLConverter) decodePart(partData *PartData) ([]byte, error) {
	transferEncoding := c.getHeader(partData.Header, "Content-Transfer-Encoding")
	
	switch strings.ToLower(transferEncoding) {
	case "quoted-printable":
		reader := quotedprintable.NewReader(bytes.NewReader(partData.Data))
		return io.ReadAll(reader)
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(partData.Data), "\n", ""))
		return decoded, err
	default:
		return partData.Data, nil
	}
}

// getHeader gets a header value from the header map
func (c *MHTMLConverter) getHeader(header map[string][]string, key string) string {
	if values, ok := header[key]; ok && len(values) > 0 {
		return values[0]
	}
	return ""
}

// walkHTML walks the HTML tree and replaces cid: references with data URLs
func (c *MHTMLConverter) walkHTML(n *html.Node, parts map[string]*PartData) {
	if n.Type == html.ElementNode {
		for i := range n.Attr {
			attr := &n.Attr[i]
			if (attr.Key == "href" || attr.Key == "src") && strings.HasPrefix(attr.Val, "cid:") {
				cid := strings.TrimPrefix(attr.Val, "cid:")
				if partData, ok := parts[cid]; ok {
					data, err := c.decodePart(partData)
					if err == nil {
						contentType := c.getHeader(partData.Header, "Content-Type")
						if contentType == "" {
							contentType = "application/octet-stream"
						}
						dataURL := fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(data))
						attr.Val = dataURL
					}
				}
			}
		}
	}

	// Recursively walk children
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		c.walkHTML(child, parts)
	}
}

// Simple decoding helper function (used in original main.go)
func DecodePartData(partData *PartData) ([]byte, error) {
	te := ""
	if values, ok := partData.Header["Content-Transfer-Encoding"]; ok && len(values) > 0 {
		te = values[0]
	}
	switch strings.ToLower(te) {
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(partData.Data)))
	case "base64":
		return base64.StdEncoding.DecodeString(strings.ReplaceAll(string(partData.Data), "\n", ""))
	default:
		return partData.Data, nil
	}
}
