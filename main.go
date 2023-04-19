package main

import (
	"context"
	"crypto/sha1"
	"embed"
	"fmt"
	"io"
	"math/rand"
	"net/http"
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
	redirectRx = regexp.MustCompile("/redirect/([^/]*)")
	cacheRx    = regexp.MustCompile("/cache/([^/]*)")
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

		// cache
		if r.URL.Path == "/cache" {
			if r.Header.Get("If-Modified-Since") != "" || r.Header.Get("If-None-Match") != "" {
				w.WriteHeader(fsthttp.StatusNotModified)
				return
			}

			lastModified := time.Now().Format(time.RFC1123)
			w.Header().Add("Last-Modified", lastModified)
			w.Header().Add("ETag", sha1hash(lastModified))
			w.Write([]byte{})
		}

		m = cacheRx.FindAllStringSubmatch(r.URL.Path, -1)
		if m != nil {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 3 {
				fsthttp.Error(w, "Not found", fsthttp.StatusNotFound)
				return
			}

			seconds, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				fsthttp.Error(w, err.Error(), fsthttp.StatusBadRequest)
				return
			}

			w.Header().Add("Cache-Control", fmt.Sprintf("public, max-age=%d", seconds))
			w.Write([]byte{})
			return
		}

		// Anything
		if r.URL.Path == "/anything" {
			w.Header().Apply(r.Header)
			io.Copy(w, r.Body)
			return
		}

		// User-agent
		if r.URL.Path == "/user-agent" {
			w.Header().Set("Content-Type", "text/json; charset=utf-8")
			fmt.Fprintf(w, `{"user-agent":"%s"}`, r.Header.Get("user-agent"))
			return
		}

		// IP
		if r.URL.Path == "/ip" {
			w.Header().Set("Content-Type", "text/json; charset=utf-8")
			fmt.Fprintf(w, `{"origin":"%s"}`, r.RemoteAddr)
			return
		}

		// Bearer
		if r.URL.Path == "/bearer" {
			reqToken := r.Header.Get("Authorization")
			tokenFields := strings.Fields(reqToken)
			if len(tokenFields) != 2 || tokenFields[0] != "Bearer" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprintf(w, `{"authenticated": true, "token":"%s"}`, tokenFields[1])
			return
		}

		// redirect
		m = redirectRx.FindAllStringSubmatch(r.URL.Path, -1)
		if m != nil {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 3 {
				fsthttp.Error(w, "Not found", fsthttp.StatusNotFound)
				return
			}

			redirects, err := strconv.Atoi(parts[2])
			if err != nil {
				fsthttp.Error(w, "Invalid redirects", fsthttp.StatusBadRequest)
				return
			}
			if redirects == 0 {
				w.Write([]byte("completed redirects"))
				return
			}
			if redirects > 20 {
				fsthttp.Error(w, "maximum of 20 redirects allowed", fsthttp.StatusBadRequest)
				return
			}
			w.Header().Set("Location", fmt.Sprintf("/redirect/%d", redirects-1))
			fsthttp.Error(w, fsthttp.StatusText(302), 302)
			return
		}

		// Unstable
		if r.URL.Path == "/unstable" {
			rate := 0.5
			rateParam := r.URL.Query().Get("failure-rate")
			pRate, err := strconv.ParseFloat(rateParam, 64)
			w.Header().Add("Surrogate-Control", "max-age=31557600")
			w.Header().Add("Cache-Control", "no-store, max-age=0")
			if err == nil {
				if pRate < 1 && pRate > 0 {
					rate = pRate
				}
			}
			if rand.Float64() > rate {
				w.Write([]byte{})
				return
			}
			fsthttp.Error(w, fsthttp.StatusText(500), 500)
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
		data, err := staticAssets.ReadFile(path.Join("static", strings.TrimLeft(r.URL.Path, "/")))
		if err == nil {
			w.Write(data)
			return
		}

		// Catch all other requests and return a 404.
		fsthttp.Error(w, fsthttp.StatusText(404), 404)
	})
}

func parseDuration(input string) (time.Duration, error) {
	d, err := time.ParseDuration(input)
	if err != nil {
		n, err := strconv.ParseFloat(input, 64)
		if err != nil {
			return 0, err
		}
		d = time.Duration(n*1000) * time.Millisecond
	}
	return d, nil
}

func parseBoundedDuration(input string, min, max time.Duration) (time.Duration, error) {
	d, err := parseDuration(input)
	if err != nil {
		return 0, err
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

func sha1hash(input string) string {
	h := sha1.New()
	return fmt.Sprintf("%x", h.Sum([]byte(input)))
}
