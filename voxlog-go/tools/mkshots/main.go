// Command mkshots writes Voxlog's own interface to standalone HTML files,
// filled with invented data, so the README can show the real thing without
// showing anybody's transcripts.
//
// The window is HTML that internal/ui splices together from the files in
// internal/ui/assets and hands to a webview, with the data arriving as one
// window.voxlog assignment and everything else as Go bindings. So the page
// renders anywhere Blink does -- which is what Chrome is -- as long as
// something plays the part of those bindings. That is all this tool is: the
// same splice, plus a script that defines window.voxlog and stubs every
// binding the page calls.
//
// It deliberately does NOT import internal/ui, which would be the obvious
// way to reuse buildMainPage: that package is cgo over Cocoa, WebKit and
// sherpa-onnx, and linking all of it into a screenshot generator to avoid
// four strings.Replace calls is a bad trade. The marker constants are
// duplicated here; internal/ui's own tests assert every one of them still
// exists in the page.
//
// Usage: go run ./tools/mkshots -out /tmp/shots
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// The markers internal/ui/assets.go splices at, and the file each one takes.
var injections = []struct{ marker, file string }{
	{"  /* KIT_CSS */", "kit.css"},
	{"  /* SETTINGS_CSS */", "settings.css"},
	{"  /* INDICATOR_CSS */", "indicator.css"},
	{"  <!-- SETTINGS_PANE -->", "settings-pane.html"},
}

func main() {
	assetsDir := flag.String("assets", "internal/ui/assets", "where the page's assets live")
	outDir := flag.String("out", "", "directory to write the pages into")
	flag.Parse()
	if *outDir == "" {
		log.Fatal("mkshots: -out is required")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	page, err := os.ReadFile(filepath.Join(*assetsDir, "main.html"))
	if err != nil {
		log.Fatal(err)
	}
	body := string(page)
	for _, in := range injections {
		asset, err := os.ReadFile(filepath.Join(*assetsDir, in.file))
		if err != nil {
			log.Fatal(err)
		}
		if !strings.Contains(body, in.marker) {
			log.Fatalf("mkshots: the page has no %q marker any more", in.marker)
		}
		body = strings.Replace(body, in.marker, string(asset), 1)
	}

	for _, shot := range shots {
		// The harness goes in right after the body opens, so every stub
		// exists before the page's own script -- which sits at the end of
		// <body> -- runs a single line. The newline in the anchor matters:
		// the word appears inside CSS comments further up the file, and
		// splicing a script into a comment is a silent no-op.
		const anchor = "\n<body>\n"
		if !strings.Contains(body, anchor) {
			log.Fatal("mkshots: the page has no <body> line to inject into")
		}
		out := strings.Replace(body, anchor, anchor+"\n<script>\n"+harness(shot)+"\n</script>", 1)
		path := filepath.Join(*outDir, shot.name+".html")
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println(path)
	}

	// The recording indicator is its own window, its own page, and one
	// marker rather than four.
	overlay, err := os.ReadFile(filepath.Join(*assetsDir, "overlay.html"))
	if err != nil {
		log.Fatal(err)
	}
	indicator, err := os.ReadFile(filepath.Join(*assetsDir, "indicator.css"))
	if err != nil {
		log.Fatal(err)
	}
	ov := strings.Replace(string(overlay), "  /* INDICATOR_CSS */", string(indicator), 1)
	ov = strings.Replace(ov, "</body>", overlayPose+"</body>", 1)
	path := filepath.Join(*outDir, "indicator.html")
	if err := os.WriteFile(path, []byte(ov), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println(path)
}

type shot struct {
	name string
	pane string
	// after runs once the page has rendered: opening a meeting, choosing a
	// settings category -- whatever the screenshot needs that a click would
	// normally do.
	after string
}

var shots = []shot{
	{name: "overview", pane: "overview"},
	{name: "history", pane: "history"},
	{name: "meetings", pane: "meetings"},
	{name: "meeting", pane: "meetings", after: `openMeeting(` + quoted(meetingID) + `);`},
	{name: "tasks", pane: "tasks"},
	{name: "settings-mcp", pane: "settings", after: `
	  document.querySelector('#settings-subnav .subnav-item[data-subpane="mcp"]').click();`},
}

func quoted(s string) string { return "'" + s + "'" }

// harness is the script that stands in for Go: the data the window would
// have been handed, and a stub for every binding the page can call.
func harness(s shot) string {
	return `
window.voxlog = ` + voxlogJSON(s.pane) + `;
window.voxlog.audioBase = '';
window.voxlog.voiceBase = '';

// webview.Bind hands the page a Promise, and several call sites .then() the
// result without checking. Every stub has to keep that shape.
(function () {
  var answers = ` + bindingAnswers + `;
  var silent = ` + silentBindings + `;
  silent.forEach(function (name) { answers[name] = null; });
  Object.keys(answers).forEach(function (name) {
    var value = answers[name];
    window[name] = function () { return Promise.resolve(value); };
  });
})();

window.addEventListener('load', function () {
  setTimeout(function () {` + s.after + `}, 60);
});
`
}

// overlayPose puts the indicator in the state worth photographing: hearing a
// voice, a few seconds in, with a shortcut to press to stop.
const overlayPose = `<script>
  // In the app this window is transparent and floats over whatever is on
  // screen. On its own it needs something to float over, or the capsule is
  // a black shape on white paper.
  document.body.style.background =
    'radial-gradient(circle at 30% 20%, #2b3a55 0%, #12151c 60%, #0a0c10 100%)';
  document.body.style.padding = '48px 0';
  window.voxlog.setShortcut('Right Command');
  window.voxlog.setElapsed(9);
  window.voxlog.setLevel(0.72);
  window.voxlog.setText('the importer should skip rows it has already seen');
</script>
`
