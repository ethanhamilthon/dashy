package acp

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func pngData(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

func TestImagePromptsReachProviderAndReplay(t *testing.T) {
	requests := make(chan string, 8)
	settings(t, func(body string) string { requests <- body; return answer("seen") })
	dir := t.TempDir()
	w := connect(t)
	w.initialize(t)
	id := w.newSession(t, 2, dir)
	data := pngData(t)
	image := func(mime, data string) map[string]string {
		return map[string]string{"type": "image", "mimeType": mime, "data": data}
	}
	invalid := [][]any{
		{image("image/gif", data)},
		{image("image/png", "not base64!")},
		{image("image/png", base64.StdEncoding.EncodeToString([]byte("not a png")))},
		{image("image/png", data), image("image/png", data), image("image/png", data), image("image/png", data), image("image/png", data)},
	}
	for i, prompt := range invalid {
		w.request(t, 10+i, "session/prompt", map[string]any{"sessionId": id, "prompt": prompt})
		if f := w.response(t, 10+i); f.Error == nil || f.Error.Code != -32602 {
			t.Fatalf("invalid prompt %d accepted: %+v", i, f)
		}
	}
	w.request(t, 3, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{
		map[string]string{"type": "text", "text": "what is this?"}, image("image/png", data)}})
	if f := w.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	body := <-requests
	if !strings.Contains(body, "input_image") || !strings.Contains(body, data) || strings.Count(body, "input_image") != 1 {
		t.Fatalf("provider did not receive exactly the image: %.400s", body)
	}
	w.request(t, 4, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{image("image/png", data)}})
	if f := w.response(t, 4); f.Error != nil {
		t.Fatal(f.Error)
	}
	<-requests
	w.updates = nil
	if f := load(t, w, 5, id, dir); f.Error != nil {
		t.Fatal(f.Error)
	}
	images := 0
	for _, raw := range w.updates {
		update := string(raw)
		if strings.Contains(update, "Original image") {
			t.Fatalf("replay leaked attachment path: %s", update)
		}
		if strings.Contains(update, `"type":"image"`) && strings.Contains(update, data) && strings.Contains(update, "image/png") {
			images++
		}
	}
	if images != 2 {
		t.Fatalf("replayed %d images", images)
	}
}
