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
