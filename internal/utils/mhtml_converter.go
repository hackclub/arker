package utils

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"golang.org/x/net/html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// StreamingPartInfo holds information about an MHTML part for streaming processing
type StreamingPartInfo struct {
	Header      map[string][]string
	ContentID   string
	Location    string
	ContentType string
	Encoding    string
	Data        []byte // Cached data for referenced parts
}

// StreamingConverter handles MHTML conversion with minimal memory usage
type StreamingConverter struct {
	partCache map[string][]byte
}

// NewStreamingConverter creates a new streaming converter
func NewStreamingConverter() *StreamingConverter {
	return &StreamingConverter{
		partCache: make(map[string][]byte),
	}
}

// ExtractMHTMLHTML returns only the first HTML root part from a Chrome MHTML
// snapshot. Historical repair needs the page's embedded schema/Open Graph
// metadata, not a fully rewritten document with every image expanded to a
// data URL, so running ConvertMHTMLToHTML would needlessly retain the whole
// archive. maxBytes bounds the decoded root document independently of the
// size of the resource bundle that follows it.
func ExtractMHTMLHTML(input io.Reader, maxBytes int64) ([]byte, error) {
	if input == nil {
		return nil, fmt.Errorf("MHTML input is nil")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("MHTML HTML byte limit must be positive")
	}
	msg, err := mail.ReadMessage(input)
	if err != nil {
		return nil, fmt.Errorf("failed to read mail message: %w", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse media type: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/related") {
		return nil, fmt.Errorf("not a multipart/related message, got: %s", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("no boundary found in content type")
	}

	mr := multipart.NewReader(msg.Body, boundary)
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			return nil, fmt.Errorf("no HTML part found")
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read multipart: %w", err)
		}
		if !strings.HasPrefix(strings.ToLower(part.Header.Get("Content-Type")), "text/html") {
			_, _ = io.Copy(io.Discard, part)
			continue
		}

		var decoded io.Reader = part
		switch strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding"))) {
		case "quoted-printable":
			decoded = quotedprintable.NewReader(part)
		case "base64":
			decoded = base64.NewDecoder(base64.StdEncoding, part)
		}
		data, err := io.ReadAll(io.LimitReader(decoded, maxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("failed to decode HTML part: %w", err)
		}
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("MHTML HTML part exceeds %d bytes", maxBytes)
		}
		return data, nil
	}
}

// ExtractMHTMLResource returns the decoded multipart resource whose
// Content-Location identifies location. Exact URLs win; provider CDN URLs may
// change resize/signature query parameters — and occasionally the delivery
// category segment — between the meta tag and image request, so the same host
// plus path or unique asset filename is the same immutable asset. Matching
// that identity is what distinguishes the post poster from avatars and
// recommendation cards.
func ExtractMHTMLResource(input io.Reader, location string, maxBytes int64) ([]byte, string, error) {
	if input == nil {
		return nil, "", fmt.Errorf("MHTML input is nil")
	}
	location = strings.TrimSpace(location)
	if location == "" {
		return nil, "", fmt.Errorf("MHTML resource location is empty")
	}
	if maxBytes <= 0 {
		return nil, "", fmt.Errorf("MHTML resource byte limit must be positive")
	}
	msg, err := mail.ReadMessage(input)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read mail message: %w", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", fmt.Errorf("failed to parse media type: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/related") {
		return nil, "", fmt.Errorf("not a multipart/related message, got: %s", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, "", fmt.Errorf("no boundary found in content type")
	}

	mr := multipart.NewReader(msg.Body, boundary)
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			return nil, "", fmt.Errorf("requested MHTML resource not found")
		}
		if err != nil {
			return nil, "", fmt.Errorf("failed to read multipart: %w", err)
		}
		if !sameMHTMLResourceLocation(part.Header.Get("Content-Location"), location) {
			_, _ = io.Copy(io.Discard, part)
			continue
		}

		var decoded io.Reader = part
		switch strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding"))) {
		case "quoted-printable":
			decoded = quotedprintable.NewReader(part)
		case "base64":
			decoded = base64.NewDecoder(base64.StdEncoding, part)
		}
		data, err := io.ReadAll(io.LimitReader(decoded, maxBytes+1))
		if err != nil {
			return nil, "", fmt.Errorf("failed to decode MHTML resource: %w", err)
		}
		if int64(len(data)) > maxBytes {
			return nil, "", fmt.Errorf("MHTML resource exceeds %d bytes", maxBytes)
		}
		contentType := part.Header.Get("Content-Type")
		if parsed, _, parseErr := mime.ParseMediaType(contentType); parseErr == nil {
			contentType = parsed
		}
		return data, contentType, nil
	}
}

func sameMHTMLResourceLocation(candidate, wanted string) bool {
	candidate, wanted = strings.TrimSpace(candidate), strings.TrimSpace(wanted)
	if candidate == wanted {
		return true
	}
	candidateURL, candidateErr := url.Parse(candidate)
	wantedURL, wantedErr := url.Parse(wanted)
	if candidateErr != nil || wantedErr != nil || candidateURL.Hostname() == "" || wantedURL.Hostname() == "" {
		return false
	}
	if !strings.EqualFold(candidateURL.Scheme, wantedURL.Scheme) || !strings.EqualFold(candidateURL.Hostname(), wantedURL.Hostname()) {
		return false
	}
	candidatePath, wantedPath := candidateURL.EscapedPath(), wantedURL.EscapedPath()
	return candidatePath != "" && wantedPath != "" &&
		(candidatePath == wantedPath || path.Base(candidatePath) == path.Base(wantedPath))
}

// ConvertMHTMLToHTML converts MHTML to HTML using streaming approach
func (sc *StreamingConverter) ConvertMHTMLToHTML(input io.Reader, output io.Writer) error {
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

	// Find all referenced CIDs and URLs in the HTML
	referencedCIDs := sc.findReferencedCIDs(htmlContent)
	referencedURLs := sc.findReferencedURLs(htmlContent)
	referencedCSSURLs := sc.findCSSURLs(htmlContent)

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
			// Check if this part matches a referenced URL
			if !isReferenced && referencedURLs[contentLocation] {
				isReferenced = true
				cacheKey = contentLocation
			}
			// Check if this part matches a referenced CSS URL
			if !isReferenced && referencedCSSURLs[contentLocation] {
				isReferenced = true
				cacheKey = contentLocation
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
				return fmt.Errorf("failed to decode part: %w", err)
			}

			// Store in part info
			if info, exists := partInfos[cacheKey]; exists {
				info.Data = decodedData
			}
		} else {
			// Skip unreferenced parts
			io.Copy(io.Discard, part)
		}
	}

	// Process HTML and replace references
	return sc.streamProcessHTML(htmlContent, partInfos, output)
}

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

func (sc *StreamingConverter) findReferencedURLs(htmlContent []byte) map[string]bool {
	referencedURLs := make(map[string]bool)
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
				if (attr.Key == "src" || attr.Key == "href") && (strings.HasPrefix(attr.Val, "http://") || strings.HasPrefix(attr.Val, "https://")) {
					referencedURLs[attr.Val] = true
				}
			}
		}
	}

	return referencedURLs
}

func (sc *StreamingConverter) findCSSURLs(htmlContent []byte) map[string]bool {
	referencedURLs := make(map[string]bool)
	tokenizer := html.NewTokenizer(bytes.NewReader(htmlContent))

	for {
		tokenType := tokenizer.Next()

		if tokenType == html.ErrorToken {
			if tokenizer.Err() == io.EOF {
				break
			}
			continue
		}

		switch tokenType {
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()

			// Check for style attributes containing url()
			for _, attr := range token.Attr {
				if attr.Key == "style" && strings.Contains(attr.Val, "url(") {
					sc.extractCSSURLsFromText(attr.Val, referencedURLs)
				}
			}

		case html.TextToken:
			// Check if this is inside a <style> tag
			token := tokenizer.Token()
			// Check if the raw text contains url( - this could be any CSS url() reference
			if strings.Contains(token.Data, "url(") {
				sc.extractCSSURLsFromText(token.Data, referencedURLs)
			}
		}
	}

	return referencedURLs
}

func (sc *StreamingConverter) extractCSSURLsFromText(cssText string, urlMap map[string]bool) {
	// Find all url() patterns in CSS text
	// This regex matches url("..."), url('...'), and url(...)
	urlPattern := `url\s*\(\s*['"]?([^'")]+)['"]?\s*\)`
	re := regexp.MustCompile(urlPattern)

	matches := re.FindAllStringSubmatch(cssText, -1)
	for _, match := range matches {
		if len(match) > 1 {
			url := strings.TrimSpace(match[1])
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				urlMap[url] = true
			}
		}
	}
}

func (sc *StreamingConverter) replaceCSSURLs(cssText string, partInfos map[string]*StreamingPartInfo) string {
	// Find all url() patterns and replace them with data URLs if we have the data
	urlPattern := `url\s*\(\s*['"]?([^'")]+)['"]?\s*\)`
	re := regexp.MustCompile(urlPattern)

	return re.ReplaceAllStringFunc(cssText, func(match string) string {
		submatches := re.FindStringSubmatch(match)
		if len(submatches) > 1 {
			url := strings.TrimSpace(submatches[1])
			if partInfo, exists := partInfos[url]; exists {
				if dataURL, err := sc.getPartAsDataURL(partInfo); err == nil {
					// Preserve the original quote style
					if strings.Contains(match, `"`) {
						return fmt.Sprintf(`url("%s")`, dataURL)
					} else if strings.Contains(match, `'`) {
						return fmt.Sprintf(`url('%s')`, dataURL)
					} else {
						return fmt.Sprintf(`url(%s)`, dataURL)
					}
				}
			}
		}
		return match // Return original if no replacement found
	})
}

// streamProcessHTML processes HTML using tokenizer and replaces references
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

			// Check for attributes that might contain cid: references or matching URLs
			modified := false
			for i := range token.Attr {
				attr := &token.Attr[i]
				if attr.Key == "src" || attr.Key == "href" {
					var partInfo *StreamingPartInfo
					var exists bool

					// First try cid: references
					if strings.HasPrefix(attr.Val, "cid:") {
						cid := strings.TrimPrefix(attr.Val, "cid:")
						partInfo, exists = partInfos[cid]
					} else {
						// Try to find part by URL
						partInfo, exists = partInfos[attr.Val]
					}

					if exists {
						// Get the cached part data and convert to data URL
						dataURL, err := sc.getPartAsDataURL(partInfo)
						if err == nil {
							attr.Val = dataURL
							modified = true
						}
					}
				} else if attr.Key == "style" {
					// Handle CSS url() references in style attributes
					newStyle := sc.replaceCSSURLs(attr.Val, partInfos)
					if newStyle != attr.Val {
						attr.Val = newStyle
						modified = true
					}
				}
			}

			// Write the token (possibly modified)
			if modified {
				output.Write([]byte(token.String()))
			} else {
				output.Write(tokenizer.Raw())
			}

		case html.TextToken:
			// Check if this might be CSS content with url() references
			token := tokenizer.Token()
			if strings.Contains(token.Data, "url(") {
				newCSS := sc.replaceCSSURLs(token.Data, partInfos)
				if newCSS != token.Data {
					output.Write([]byte(newCSS))
				} else {
					output.Write(tokenizer.Raw())
				}
			} else {
				output.Write(tokenizer.Raw())
			}

		default:
			// Write raw token data
			output.Write(tokenizer.Raw())
		}
	}
}

// getPartAsDataURL converts a part to a data URL
func (sc *StreamingConverter) getPartAsDataURL(partInfo *StreamingPartInfo) (string, error) {
	if partInfo.Data == nil {
		return "", fmt.Errorf("part data not loaded")
	}

	// Encode as base64 data URL
	encoded := base64.StdEncoding.EncodeToString(partInfo.Data)
	dataURL := fmt.Sprintf("data:%s;base64,%s", partInfo.ContentType, encoded)

	return dataURL, nil
}

// decodePart decodes a part based on its Content-Transfer-Encoding
func (sc *StreamingConverter) decodePart(data []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "base64":
		return base64.StdEncoding.DecodeString(string(data))
	case "quoted-printable":
		reader := quotedprintable.NewReader(bytes.NewReader(data))
		return io.ReadAll(reader)
	case "7bit", "8bit", "binary", "":
		return data, nil
	default:
		return nil, fmt.Errorf("unsupported encoding: %s", encoding)
	}
}
