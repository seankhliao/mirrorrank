package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

type Mirror struct {
	u string
	d time.Duration
}

func main() {
	var ip4, ip6 bool
	var countries []string
	var file, save string
	var parallel, limit int
	exclude := map[string]struct{}{
		"checkdomain.de": {},
	}
	flag.BoolVar(&ip4, "4", false, "limit to IPv4")
	flag.BoolVar(&ip6, "6", false, "limit to IPv6")
	flag.StringVar(&file, "f", "", "mirrorlist to use instead of from archlinux.org/mirrorlist/")
	flag.StringVar(&save, "s", "/etc/pacman.d/mirrorlist", "output file location")
	flag.IntVar(&parallel, "p", 10, "parallel downloads")
	flag.IntVar(&limit, "l", 5, "limit output")
	flag.Func("e", "exclude string (repeatable)", func(s string) error {
		exclude[s] = struct{}{}
		return nil
	})
	flag.Func("c", "limit to countries (repeatable)", func(s string) error {
		countries = append(countries, s)
		return nil
	})
	flag.Parse()

	lg := slog.New(slog.NewTextHandler(os.Stdout, nil))

	client := http.Client{
		Timeout: 5 * time.Second,
	}

	// Get raw mirror list
	var rawMirrorlist io.Reader
	if file == "" {
		v := url.Values{}
		v.Add("protocol", "https")
		v.Add("use_mirror_status", "on")
		if ip4 {
			v.Add("ip_version", "4")
		}
		if ip6 {
			v.Add("ip_version", "6")
		}
		for _, c := range countries {
			v.Add("country", strings.ToUpper(c))
		}
		u := url.URL{
			Scheme:   "https",
			Host:     "archlinux.org",
			Path:     "/mirrorlist/",
			RawQuery: v.Encode(),
		}

		r, err := client.Get(u.String())
		if err != nil {
			lg.Error("get mirrorlist", "err", err, "source", u.String())
			return
		}
		defer r.Body.Close()
		rawMirrorlist = r.Body
	} else {
		var err error
		rawMirrorlist, err = os.Open(file)
		if err != nil {
			lg.Error("get mirrorlist file", "err", err, "file", file)
			return
		}
	}

	// Parse raw mirror list
	var rawMirrors []string
	scanner := bufio.NewScanner(rawMirrorlist)
loop:
	for scanner.Scan() {
		s := strings.TrimPrefix(scanner.Text(), "#")
		s = strings.TrimSpace(s)
		var mirror string
		_, err := fmt.Sscanf(s, "Server = %s", &mirror)
		if err != nil {
			continue
		}
		for s := range exclude {
			if strings.Contains(mirror, s) {
				continue loop
			}
		}
		rawMirrors = append(rawMirrors, mirror)
	}

	lg.Info("got candidates", "mirrors", len(rawMirrors))

	// rank mirrors
	collect := make(chan Mirror)
	done := make(chan []Mirror)
	go func() {
		mirrors := make([]Mirror, 0, len(rawMirrors))
		for m := range collect {
			mirrors = append(mirrors, m)
		}
		sort.Slice(mirrors, func(i, j int) bool { return mirrors[i].d.Milliseconds() < mirrors[j].d.Milliseconds() })
		done <- mirrors
	}()
	ch := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	replacer := strings.NewReplacer("$repo", "extra", "$arch", "x86_64")
	for i := range rawMirrors {
		ch <- struct{}{}
		wg.Add(1)
		go func(m string) {
			var err error
			defer func() {
				if err != nil {
					if strings.Contains(err.Error(), "context deadline exceeded") {
						// not wrapped?
						err = errors.New("timeout")
					}
					lg.Error("download extra.db", "err", err, "mirror", m)
				}
				<-ch
				wg.Done()
			}()
			u := replacer.Replace(m + "/extra.db")
			t := time.Now()
			r, err := client.Get(u)
			if err != nil {
				return
			}
			defer r.Body.Close()
			_, err = io.Copy(io.Discard, r.Body)
			if err != nil {
				return
			}
			s := time.Since(t)
			collect <- Mirror{u: m, d: s}
			lg.Info("done", "time", s.Round(time.Millisecond), "mirror", m)
		}(rawMirrors[i])
	}
	wg.Wait()
	close(collect)

	mirrors := <-done

	// output mirrors
	bi, _ := debug.ReadBuildInfo()
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("## Generated by %s @ %s on %v\n", bi.Main.Path, bi.Main.Version, time.Now()))
	b.WriteString(fmt.Sprintf("## %s\n\n", strings.Join(os.Args, " ")))
	for _, m := range mirrors[:limit] {
		b.WriteString(fmt.Sprintf("Server = %s\n", m.u))
	}
	err := os.WriteFile(save, b.Bytes(), 0o644)
	if err != nil {
		lg.Error("write file", "err", err, "file", save)
		return
	}
}
