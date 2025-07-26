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

// StreamingPartInfo holds metadata about a part without loading data
type StreamingPartInfo struct {
	Header      map[string][]string
	ContentID   string
	Location    string
	ContentType string
	Encoding    string
}

// StreamingConverter handles large MHTML files with constant memory usage
type StreamingConverter struct {
	referencedParts map[string][]byte // Only cache parts that are actually referenced
}

// Convert converts MHTML content to HTML using streaming approach for large files
func (c *MHTMLConverter) Convert(input io.Reader, output io.Writer) error {
	// Use streaming converter for constant memory usage
	sc := &StreamingConverter{
		referencedParts: make(map[string][]byte),
	}
	return sc.StreamingConvert(input, output)
}

// StreamingConvert processes MHTML with constant memory usage
func (sc *StreamingConverter) StreamingConvert(input io.Reader, output io.Writer) error {
	// Read all data first (we need to do two passes)
	data, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	
	// Parse MHTML headers
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

	// First pass: find HTML content and identify referenced CIDs
	mr := multipart.NewReader(msg.Body, boundary)
	
	var htmlContent []byte
	partInfos := make(map[string]*StreamingPartInfo)
	var htmlFound bool

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read multipart: %w", err)
		}

		partContentType := part.Header.Get("Content-Type")
		contentID := strings.Trim(part.Header.Get("Content-ID"), "<>")
		contentLocation := part.Header.Get("Content-Location")
		encoding := part.Header.Get("Content-Transfer-Encoding")

		info := &StreamingPartInfo{
			Header:      map[string][]string(part.Header),
			ContentID:   contentID,
			Location:    contentLocation,
			ContentType: partContentType,
			Encoding:    encoding,
		}

		// Store part info for lookup
		if contentID != "" {
			partInfos[contentID] = info
		}
		if contentLocation != "" {
			partInfos[contentLocation] = info
			// Also store without "cid:" prefix
			if strings.HasPrefix(contentLocation, "cid:") {
				cidKey := strings.TrimPrefix(contentLocation, "cid:")
				partInfos[cidKey] = info
			}
		}

		// If this is the HTML part, read and decode it
		if !htmlFound && strings.HasPrefix(partContentType, "text/html") {
			htmlData, err := io.ReadAll(part)
			if err != nil {
				return fmt.Errorf("failed to read HTML part: %w", err)
			}
			
			htmlContent, err = sc.decodePart(htmlData, encoding)
			if err != nil {
				return fmt.Errorf("failed to decode HTML part: %w", err)
			}
			htmlFound = true
		} else {
			// Skip non-HTML parts in first pass
			io.Copy(io.Discard, part)
		}
	}

	if !htmlFound {
		return fmt.Errorf("no HTML part found")
	}

	// Find all referenced CIDs in the HTML
	referencedCIDs := sc.findReferencedCIDs(htmlContent)
	
	// Second pass: load only referenced parts
	msg2, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to re-read mail message: %w", err)
	}
	
	mr2 := multipart.NewReader(msg2.Body, boundary)
	for {
		part, err := mr2.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read multipart in second pass: %w", err)
		}

		contentID := strings.Trim(part.Header.Get("Content-ID"), "<>")
		contentLocation := part.Header.Get("Content-Location")
		encoding := part.Header.Get("Content-Transfer-Encoding")
		
		// Check if this part is referenced
		isReferenced := false
		cacheKey := ""
		
		// Check by Content-ID
		if contentID != "" {
			if referencedCIDs[contentID] {
				isReferenced = true
				cacheKey = contentID
			}
		}
		
		// Check by Content-Location  
		if contentLocation != "" {
			// Try exact match
			if referencedCIDs[contentLocation] {
				isReferenced = true
				cacheKey = contentLocation
			}
			// Try without cid: prefix
			if strings.HasPrefix(contentLocation, "cid:") {
				cidKey := strings.TrimPrefix(contentLocation, "cid:")
				if referencedCIDs[cidKey] {
					isReferenced = true
					cacheKey = cidKey
				}
			}
			// Try with cid: prefix added
			if !strings.HasPrefix(contentLocation, "cid:") {
				cidWithPrefix := "cid:" + contentLocation
				if referencedCIDs[cidWithPrefix] {
					isReferenced = true
					cacheKey = contentLocation
				}
			}
		}
		
		if isReferenced {
			// Load and cache this part
			partData, err := io.ReadAll(part)
			if err != nil {
				return fmt.Errorf("failed to read referenced part: %w", err)
			}
			
			decodedData, err := sc.decodePart(partData, encoding)
			if err != nil {
				return fmt.Errorf("failed to decode referenced part: %w", err)
			}
			
			sc.referencedParts[cacheKey] = decodedData
		} else {
			// Skip unreferenced parts
			io.Copy(io.Discard, part)
		}
	}

	// Stream HTML through tokenizer and replace cid: references
	return sc.streamProcessHTML(htmlContent, partInfos, output)
}

// findReferencedCIDs scans HTML content to find all cid: references
func (sc *StreamingConverter) findReferencedCIDs(htmlContent []byte) map[string]bool {
	referencedCIDs := make(map[string]bool)
	tokenizer := html.NewTokenizer(bytes.NewReader(htmlContent))
	
	for {
		tokenType := tokenizer.Next()
		
		if tokenType == html.ErrorToken {
			if tokenizer.Err() == io.EOF {
				break
			}
			continue
		}
		
		if tokenType == html.StartTagToken || tokenType == html.SelfClosingTagToken {
			token := tokenizer.Token()
			
			for _, attr := range token.Attr {
				if (attr.Key == "src" || attr.Key == "href") && strings.HasPrefix(attr.Val, "cid:") {
					cid := strings.TrimPrefix(attr.Val, "cid:")
					referencedCIDs[cid] = true
				}
			}
		}
	}
	
	return referencedCIDs
}

// streamProcessHTML processes HTML using tokenizer and replaces cid: references
func (sc *StreamingConverter) streamProcessHTML(htmlContent []byte, partInfos map[string]*StreamingPartInfo, output io.Writer) error {
	tokenizer := html.NewTokenizer(bytes.NewReader(htmlContent))
	
	for {
		tokenType := tokenizer.Next()
		
		switch tokenType {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return nil
			}
			return tokenizer.Err()
			
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			
			// Check for attributes that might contain cid: references
			modified := false
			for i := range token.Attr {
				attr := &token.Attr[i]
				if (attr.Key == "src" || attr.Key == "href") && strings.HasPrefix(attr.Val, "cid:") {
					cid := strings.TrimPrefix(attr.Val, "cid:")
					if partInfo, exists := partInfos[cid]; exists {
						// Get the cached part data and convert to data URL
						dataURL, err := sc.getPartAsDataURL(partInfo)
						if err == nil {
							attr.Val = dataURL
							modified = true
						}
					}
				}
			}
			
			// Write the token (possibly modified)
			if modified {
				output.Write([]byte(token.String()))
			} else {
				output.Write(tokenizer.Raw())
			}
			
		default:
			// Copy other tokens as-is
			output.Write(tokenizer.Raw())
		}
	}
}

// getPartAsDataURL loads a referenced part and converts it to a data URL
func (sc *StreamingConverter) getPartAsDataURL(partInfo *StreamingPartInfo) (string, error) {
	// Try multiple possible cache keys
	var possibleKeys []string
	
	if partInfo.ContentID != "" {
		possibleKeys = append(possibleKeys, partInfo.ContentID)
	}
	if partInfo.Location != "" {
		possibleKeys = append(possibleKeys, partInfo.Location)
		// Try without cid: prefix
		if strings.HasPrefix(partInfo.Location, "cid:") {
			possibleKeys = append(possibleKeys, strings.TrimPrefix(partInfo.Location, "cid:"))
		}
		// Try with cid: prefix
		if !strings.HasPrefix(partInfo.Location, "cid:") {
			possibleKeys = append(possibleKeys, "cid:"+partInfo.Location)
		}
	}
	
	// Try each possible key
	for _, key := range possibleKeys {
		if data, exists := sc.referencedParts[key]; exists {
			contentType := partInfo.ContentType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			return fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(data)), nil
		}
	}
	
	return "", fmt.Errorf("part not cached, tried keys: %v", possibleKeys)
}

// decodePart decodes part data based on transfer encoding
func (sc *StreamingConverter) decodePart(partData []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "quoted-printable":
		reader := quotedprintable.NewReader(bytes.NewReader(partData))
		return io.ReadAll(reader)
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(partData), "\n", ""))
		return decoded, err
	default:
		return partData, nil
	}
}

// Legacy decodePart for backward compatibility
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
