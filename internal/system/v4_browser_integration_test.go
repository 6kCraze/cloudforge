//go:build integration

package system_test

import (
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/httpapi"
	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
)

// Opt-in human/browser validation against the same guarded isolated targets.
// No development configuration fallback, application debug routes or new service.
func TestV4BrowserSession(t *testing.T) {
	if os.Getenv("CLOUDFORGE_BROWSER_DEMO") != "1" {
		t.Skip("opt-in isolated browser session")
	}
	stopFile := os.Getenv("CLOUDFORGE_BROWSER_STOP_FILE")
	if !filepath.IsAbs(stopFile) {
		t.Fatal("absolute temporary stop-file path required")
	}
	f := newV3Fixture(t)
	root := t.TempDir()
	storage, err := images.Open(filepath.Join(root, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(httpapi.NewWithImages(jobs.NewService(f.jobs), f.pool, f.logger, storage).Handler())
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	f.startWorker(t, "v4-browser-worker", f.jobs, storage.Execute, func() {})
	f.waitForConsumers(t, 1)
	startV4Dispatcher(t, f)
	for index, name := range []string{"landscape.jpg", "transparency.png", "portrait.png", "small-icon.png"} {
		w, h := 640, 320
		if index == 1 {
			w, h = 240, 480
		}
		if index == 2 {
			w, h = 160, 240
		}
		if index == 3 {
			w, h = 48, 48
		}
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		colors := []color.NRGBA{{R: 109, G: 146, B: 132, A: 255}, {R: 173, G: 148, B: 96, A: 96}, {R: 90, G: 116, B: 155, A: 255}, {R: 197, G: 128, B: 107, A: 255}}
		base := colors[index]
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := base
				if y > h/2+x/5 {
					c.R /= 2
					c.G = uint8(int(c.G) * 3 / 4)
					c.B /= 2
				}
				img.SetNRGBA(x, y, c)
			}
		}
		file, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			err = jpeg.Encode(file, img, nil)
		} else {
			err = png.Encode(file, img)
		}
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("BROWSER_URL=%s FIXTURES=%s", server.URL, root)
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			t.Log("browser session stop requested; closing isolated runtime")
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("browser session timed out")
}
