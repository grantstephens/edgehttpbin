package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"math/rand"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
)

//go:embed static/*
var staticAssets embed.FS

var (
	statusRx   = regexp.MustCompile("/status/([^/]*)")
	delayRx    = regexp.MustCompile("/delay/([^/]*)")
	bytesRx    = regexp.MustCompile("/bytes/([^/]*)")
	anythingRx = regexp.MustCompile("/anything")
)

func main() {
	rand.Seed(time.Now().Unix())
	fsthttp.ServeFunc(func(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
		// Status
		m := statusRx.FindAllStringSubmatch(r.URL.Path, -1)
		if m != nil {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 3 {
				fsthttp.Error(w, "Not found", fsthttp.StatusNotFound)
				return
			}
			codes := strings.Split(parts[2], ",")
			if len(codes) > 1 {
				// Don't cache for random responses
				w.Header().Add("Surrogate-Control", "max-age=31557600")
				w.Header().Add("Cache-Control", "no-store, max-age=0")
			}
			code, err := strconv.Atoi(codes[rand.Intn(len(codes))])
			if err != nil {
				fsthttp.Error(w, "Invalid status", fsthttp.StatusBadRequest)
				return
			}
			if code >= 300 {
				fsthttp.Error(w, fsthttp.StatusText(code), code)
				return
			}
			if code > 999 {
				fsthttp.Error(w, fsthttp.StatusText(400), 400)
			}
			w.WriteHeader(code)

			return
		}

		// Delay
		m = delayRx.FindAllStringSubmatch(r.URL.Path, -1)
		if m != nil {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 3 {
				fsthttp.Error(w, "Not found", fsthttp.StatusNotFound)
				return
			}

			delay, err := parseBoundedDuration(parts[2], 0, time.Minute)
			if err != nil {
				fsthttp.Error(w, "Invalid duration", fsthttp.StatusBadRequest)
				return
			}

			select {
			case <-ctx.Done():
				w.WriteHeader(499) // "Client Closed Request" https://httpstatuses.com/499
				return
			case <-time.After(delay):
				w.Write([]byte("delayed ok"))
				return
			}
		}

		// Bytes
		m = bytesRx.FindAllStringSubmatch(r.URL.Path, -1)
		if m != nil {
			handleBytes(w, r)
			return
		}

		// Anything
		m = anythingRx.FindAllStringSubmatch(r.URL.Path, -1)
		if m != nil {
			w.Header().Apply(r.Header)
			io.Copy(w, r.Body)
			return
		}

		// Index
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			data, err := staticAssets.ReadFile(path.Join("static", "index.html"))
			if err != nil {
				fsthttp.Error(w, fsthttp.StatusText(500), 500)
			}
			w.Write(data)
			return
		}

		// Catch all other requests and return a 404.
		fsthttp.Error(w, fsthttp.StatusText(404), 404)
	})
}

func parseBoundedDuration(input string, min, max time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(input)
	if err != nil {
		n, err := strconv.ParseFloat(input, 64)
		if err != nil {
			return 0, err
		}
		d = time.Duration(n*1000) * time.Millisecond
	}

	if d > max {
		err = fmt.Errorf("duration %s longer than %s", d, max)
	} else if d < min {
		err = fmt.Errorf("duration %s shorter than %s", d, min)
	}
	return d, err
}

func handleBytes(w fsthttp.ResponseWriter, r *fsthttp.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 3 {
		fsthttp.Error(w, "Not found", fsthttp.StatusNotFound)
		return
	}

	numBytes, err := strconv.Atoi(parts[2])
	if err != nil {
		fsthttp.Error(w, err.Error(), fsthttp.StatusBadRequest)
		return
	}

	if numBytes < 0 {
		fsthttp.Error(w, "Bad Request", fsthttp.StatusBadRequest)
		return
	}

	// Special case 0 bytes and exit early, since streaming & chunk size do not
	// matter here.
	if numBytes == 0 {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(fsthttp.StatusOK)
		return
	}

	if numBytes > 100*1024 {
		numBytes = 100 * 1024
	}

	writer := func(chunk []byte) {
		w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
		w.Write(chunk)
	}

	w.Header().Set("Content-Type", "application/octet-stream")

	var chunk []byte
	for i := 0; i < numBytes; i++ {
		chunk = append(chunk, byte(rand.Intn(256)))
		if len(chunk) == numBytes {
			writer(chunk)
			chunk = nil
		}
	}
	if len(chunk) > 0 {
		writer(chunk)
	}
}
