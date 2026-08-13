package handlers

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"arker/internal/models"
)

func TestDisplayVideoMetadataScriptExecutes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the rendered viewer JavaScript smoke test")
	}

	templatePath := filepath.Join("..", "..", "templates", "display_type.html")
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		t.Fatalf("parse display template: %v", err)
	}

	var rendered bytes.Buffer
	item := models.ArchiveItem{Type: "yt-dlp", Status: "completed", Extension: ".mp4"}
	err = tmpl.ExecuteTemplate(&rendered, "display_type.html", map[string]interface{}{
		"date":              time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC).Format(time.RFC1123),
		"timestamp":         "2026-08-12T12:00:00Z",
		"tabs":              []archiveTab{{URLType: "yt-dlp", DisplayName: "Video", Status: "completed", IsActive: true}},
		"current_item":      &item,
		"current_type":      "yt-dlp",
		"short_id":          "vid01",
		"host":              "archive.example.com",
		"original_url":      "https://www.youtube.com/watch?v=fixture",
		"git_repo_name":     "",
		"download_filename": "fixture.mp4",
		"queue_position":    0,
	})
	if err != nil {
		t.Fatalf("render display template: %v", err)
	}

	scriptPattern := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	match := scriptPattern.FindSubmatch(rendered.Bytes())
	if len(match) != 2 {
		t.Fatal("rendered viewer did not contain its inline script")
	}

	harness := fmt.Sprintf(`
const hostConsole = globalThis.console;
const errors = [];
const console = {
  error: (...args) => errors.push(args.map(String).join(' ')),
  log: (...args) => hostConsole.log(...args),
};

class Element {
  constructor(tagName) {
    this.tagName = tagName.toUpperCase();
    this.childNodes = [];
    this.style = {};
    this.className = '';
    this._textContent = '';
    this._innerHTML = '';
    this.classList = { add() {}, remove() {} };
  }
  appendChild(child) { this.childNodes.push(child); return child; }
  set textContent(value) { this._textContent = String(value); this.childNodes = []; }
  get textContent() { return this._textContent; }
  set innerHTML(value) { this._innerHTML = String(value); this.childNodes = []; }
  get innerHTML() { return this._innerHTML; }
  setAttribute() {}
  focus() {}
  select() {}
}

const elements = new Map([
  ['archive-time', new Element('span')],
  ['past-archives-list', new Element('div')],
  ['video-meta', new Element('div')],
]);
let onDOMContentLoaded;
const document = {
  body: new Element('body'),
  createElement: tagName => new Element(tagName),
  getElementById: id => elements.get(id) || null,
  addEventListener: (event, callback) => {
    if (event === 'DOMContentLoaded') onDOMContentLoaded = callback;
  },
  execCommand: () => true,
};
const window = { location: { protocol: 'https:', host: 'archive.example.com' } };
const navigator = { clipboard: { writeText: async () => {} } };
const location = { reload() {} };
const setInterval = () => 1;
const clearInterval = () => {};

async function fetch(url) {
  if (url === '/video/vid01/manifest') {
    return {
      ok: true,
      status: 200,
      json: async () => ({
        metadata_available: true,
        metadata: {
          title: 'Fixture title',
          author: 'Fixture author',
          platform: 'youtube',
          publication_timestamp: '2026-08-12T11:00:00Z',
          duration_seconds: 65,
          engagement: { views: 12 },
          provenance: 'native',
          description: 'Fixture description',
          tags: ['fixture'],
        },
      }),
    };
  }
  if (url.startsWith('/web/past-archives?')) {
    return { ok: true, status: 200, json: async () => [] };
  }
  throw new Error('unexpected fetch: ' + url);
}

%s

(async () => {
  if (typeof onDOMContentLoaded !== 'function') {
    throw new Error('DOMContentLoaded handler was not registered');
  }
  onDOMContentLoaded();
  await new Promise(resolve => globalThis.setTimeout(resolve, 20));

  const meta = elements.get('video-meta');
  const title = meta.childNodes.find(child => child.className === 'video-title');
  if (!title || title.textContent !== 'Fixture title') {
    throw new Error('video metadata was not rendered: ' + JSON.stringify({
      text: meta.textContent,
      children: meta.childNodes.map(child => ({ className: child.className, text: child.textContent })),
    }));
  }
  if (errors.length) throw new Error('viewer logged errors: ' + errors.join(' | '));
})().catch(error => {
  hostConsole.error(error.stack || String(error));
  process.exitCode = 1;
});
`, string(match[1]))

	scriptPath := filepath.Join(t.TempDir(), "display-video-smoke.js")
	if err := os.WriteFile(scriptPath, []byte(harness), 0o600); err != nil {
		t.Fatalf("write JavaScript harness: %v", err)
	}
	output, err := exec.Command(node, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("rendered viewer JavaScript failed: %v\n%s", err, output)
	}
}

// renderViewerScript renders the display template for one tab and returns its
// inline script, so a harness can execute exactly what a browser would.
func renderViewerScript(t *testing.T, archiveType, shortID string) string {
	t.Helper()
	templatePath := filepath.Join("..", "..", "templates", "display_type.html")
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		t.Fatalf("parse display template: %v", err)
	}
	var rendered bytes.Buffer
	item := models.ArchiveItem{Type: archiveType, Status: "completed", Extension: ".zip"}
	err = tmpl.ExecuteTemplate(&rendered, "display_type.html", map[string]interface{}{
		"date":              time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC).Format(time.RFC1123),
		"timestamp":         "2026-08-12T12:00:00Z",
		"tabs":              []archiveTab{{URLType: archiveType, DisplayName: "Gallery", Status: "completed", IsActive: true}},
		"current_item":      &item,
		"current_type":      archiveType,
		"short_id":          shortID,
		"host":              "archive.example.com",
		"original_url":      "https://www.instagram.com/p/fixture/",
		"git_repo_name":     "",
		"download_filename": "fixture.zip",
		"queue_position":    0,
	})
	if err != nil {
		t.Fatalf("render display template: %v", err)
	}
	match := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindSubmatch(rendered.Bytes())
	if len(match) != 2 {
		t.Fatal("rendered viewer did not contain its inline script")
	}
	return string(match[1])
}

// An incomplete capture that renders identically to a complete one is the
// failure this badge exists to prevent, so the rendered page is checked by
// running its own JavaScript rather than by grepping the template.
func TestDisplayGalleryCompletenessBadge(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the rendered viewer JavaScript smoke test")
	}
	script := renderViewerScript(t, "gallery-dl", "gal01")

	for _, tc := range []struct {
		name         string
		completeness string
		wantBadge    string
	}{
		{"partial with a known count", `"completeness": {"state": "partial", "expected": 4, "stored": 1, "missing_indices": [2,3,4]}`, "1 of 4"},
		{"partial with no count", `"completeness": {"state": "partial", "stored": 1}`, "failed partway"},
		{"unknown", `"completeness": {"state": "unknown", "stored": 1}`, "Completeness unknown"},
		// A complete post, and an archive written before the field existed,
		// must both render without a badge — one because there is nothing to
		// report, the other because flagging every old archive is noise.
		{"complete", `"completeness": {"state": "complete", "expected": 1, "stored": 1}`, ""},
		{"legacy bundle with no record", `"file_count": 1`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := fmt.Sprintf(`
const hostConsole = globalThis.console;
const errors = [];
const console = { error: (...a) => errors.push(a.map(String).join(' ')), log: (...a) => hostConsole.log(...a) };

class Element {
  constructor(tagName) {
    this.tagName = tagName.toUpperCase();
    this.childNodes = [];
    this.style = {};
    this.className = '';
    this._textContent = '';
    this.classList = { add() {}, remove() {} };
  }
  appendChild(child) { this.childNodes.push(child); return child; }
  set textContent(v) { this._textContent = String(v); this.childNodes = []; }
  get textContent() { return this._textContent; }
  set innerHTML(v) { this._innerHTML = String(v); this.childNodes = []; }
  get innerHTML() { return this._innerHTML; }
  setAttribute() {}
  focus() {}
  select() {}
}

const elements = new Map([
  ['archive-time', new Element('span')],
  ['past-archives-list', new Element('div')],
  ['gallery-meta', new Element('div')],
  ['gallery-items', new Element('div')],
]);
let onDOMContentLoaded;
const document = {
  body: new Element('body'),
  createElement: tag => new Element(tag),
  getElementById: id => elements.get(id) || null,
  addEventListener: (e, cb) => { if (e === 'DOMContentLoaded') onDOMContentLoaded = cb; },
  execCommand: () => true,
};
const window = { location: { protocol: 'https:', host: 'archive.example.com' } };
const navigator = { clipboard: { writeText: async () => {} } };
const location = { reload() {} };
const setInterval = () => 1;
const clearInterval = () => {};

async function fetch(url) {
  if (url === '/gallery/gal01/list') {
    return { ok: true, status: 200, json: async () => ({
      short_id: 'gal01',
      metadata: { author: 'someone', extractor: 'instagram', description: 'caption', %s },
      files: [{ name: '001.jpg', content_type: 'image/jpeg', url: '/gallery/gal01/file/001.jpg' }],
    }) };
  }
  if (url.startsWith('/web/past-archives?')) return { ok: true, status: 200, json: async () => [] };
  throw new Error('unexpected fetch: ' + url);
}

%s

(async () => {
  if (typeof onDOMContentLoaded !== 'function') throw new Error('DOMContentLoaded handler was not registered');
  onDOMContentLoaded();
  await new Promise(resolve => globalThis.setTimeout(resolve, 20));

  const meta = elements.get('gallery-meta');
  const badge = meta.childNodes.find(child => child.className === 'gallery-completeness');
  const want = %q;
  if (want === '') {
    if (badge) throw new Error('unexpected completeness badge: ' + badge.textContent);
  } else {
    if (!badge) throw new Error('no completeness badge rendered; children: ' + JSON.stringify(meta.childNodes.map(c => c.className)));
    if (!badge.textContent.includes(want)) throw new Error('badge text = ' + badge.textContent);
  }
  // The media itself must still render: the badge labels the archive, it does
  // not replace it.
  if (!elements.get('gallery-items').childNodes.length) throw new Error('media was not rendered');
  if (errors.length) throw new Error('viewer logged errors: ' + errors.join(' | '));
})().catch(error => { hostConsole.error(error.stack || String(error)); process.exitCode = 1; });
`, tc.completeness, script, tc.wantBadge)

			scriptPath := filepath.Join(t.TempDir(), "display-gallery-smoke.js")
			if err := os.WriteFile(scriptPath, []byte(harness), 0o600); err != nil {
				t.Fatalf("write JavaScript harness: %v", err)
			}
			if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
				t.Fatalf("rendered viewer JavaScript failed: %v\n%s", err, output)
			}
		})
	}
}
